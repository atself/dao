package container

import (
	"fmt"
	"io"
	"net/http/httputil"
	"strings"

	"golang.org/x/net/context"

	"github.com/docker/docker/api/client"
	"github.com/docker/docker/cli"
	"github.com/docker/docker/pkg/promise"
	"github.com/docker/docker/pkg/signal"
	"github.com/docker/engine-api/types"
	"github.com/spf13/cobra"
)

type startOptions struct {
	attach     bool
	openStdin  bool
	detachKeys string

	containers []string
}

// NewStartCommand creats a new cobra.Command for `docker start`
func NewStartCommand(dockerCli *client.DockerCli) *cobra.Command {
	var opts startOptions

	cmd := &cobra.Command{
		Use:   "start [OPTIONS] CONTAINER [CONTAINER...]",
		Short: "启动一个或多个停止的容器",
		Args:  cli.RequiresMinArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.containers = args
			return runStart(dockerCli, &opts)
		},
	}

	flags := cmd.Flags()
	flags.BoolVarP(&opts.attach, "attach", "a", false, "附件标准输出/标准错误，同时转发信号")
	flags.BoolVarP(&opts.openStdin, "interactive", "i", false, "附加容器的标准输入")
	flags.StringVar(&opts.detachKeys, "detach-keys", "", "覆盖从一个容器退出附加操作时的按键顺序")
	return cmd
}

func runStart(dockerCli *client.DockerCli, opts *startOptions) error {
	ctx, cancelFun := context.WithCancel(context.Background())

	if opts.attach || opts.openStdin {
		// We're going to attach to a container.
		// 1. Ensure we only have one container.
		if len(opts.containers) > 1 {
			return fmt.Errorf("您不能一次性启动和附加到多个容器。")
		}

		// 2. Attach to the container.
		container := opts.containers[0]
		c, err := dockerCli.Client().ContainerInspect(ctx, container)
		if err != nil {
			return err
		}

		// We always use c.ID instead of container to maintain consistency during `docker start`
		if !c.Config.Tty {
			sigc := dockerCli.ForwardAllSignals(ctx, c.ID)
			defer signal.StopCatch(sigc)
		}

		if opts.detachKeys != "" {
			dockerCli.ConfigFile().DetachKeys = opts.detachKeys
		}

		options := types.ContainerAttachOptions{
			Stream:     true,
			Stdin:      opts.openStdin && c.Config.OpenStdin,
			Stdout:     true,
			Stderr:     true,
			DetachKeys: dockerCli.ConfigFile().DetachKeys,
		}

		var in io.ReadCloser

		if options.Stdin {
			in = dockerCli.In()
		}

		resp, errAttach := dockerCli.Client().ContainerAttach(ctx, c.ID, options)
		if errAttach != nil && errAttach != httputil.ErrPersistEOF {
			// ContainerAttach return an ErrPersistEOF (connection closed)
			// means server met an error and put it in Hijacked connection
			// keep the error and read detailed error message from hijacked connection
			return errAttach
		}
		defer resp.Close()
		cErr := promise.Go(func() error {
			errHijack := dockerCli.HoldHijackedConnection(ctx, c.Config.Tty, in, dockerCli.Out(), dockerCli.Err(), resp)
			if errHijack == nil {
				return errAttach
			}
			return errHijack
		})

		// 3. Start the container.
		if err := dockerCli.Client().ContainerStart(ctx, c.ID, types.ContainerStartOptions{}); err != nil {
			cancelFun()
			<-cErr
			return err
		}

		// 4. Wait for attachment to break.
		if c.Config.Tty && dockerCli.IsTerminalOut() {
			if err := dockerCli.MonitorTtySize(ctx, c.ID, false); err != nil {
				fmt.Fprintf(dockerCli.Err(), "监视终端大小出错: %s\n", err)
			}
		}
		if attchErr := <-cErr; attchErr != nil {
			return attchErr
		}
		_, status, err := getExitCode(dockerCli, ctx, c.ID)
		if err != nil {
			return err
		}
		if status != 0 {
			return cli.StatusError{StatusCode: status}
		}
	} else {
		// We're not going to attach to anything.
		// Start as many containers as we want.
		return startContainersWithoutAttachments(dockerCli, ctx, opts.containers)
	}

	return nil
}

func startContainersWithoutAttachments(dockerCli *client.DockerCli, ctx context.Context, containers []string) error {
	var failedContainers []string
	for _, container := range containers {
		if err := dockerCli.Client().ContainerStart(ctx, container, types.ContainerStartOptions{}); err != nil {
			fmt.Fprintf(dockerCli.Err(), "%s\n", err)
			failedContainers = append(failedContainers, container)
		} else {
			fmt.Fprintf(dockerCli.Out(), "%s\n", container)
		}
	}

	if len(failedContainers) > 0 {
		return fmt.Errorf("错误: 启动容器失败:  %v", strings.Join(failedContainers, ", "))
	}
	return nil
}

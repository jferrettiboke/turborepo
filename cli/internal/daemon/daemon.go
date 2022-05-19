package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/fatih/color"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/cli"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/vercel/turborepo/cli/internal/config"
	"github.com/vercel/turborepo/cli/internal/fs"
	"github.com/vercel/turborepo/cli/internal/server"
	"github.com/vercel/turborepo/cli/internal/ui"
	"github.com/vercel/turborepo/cli/internal/util"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Command struct {
	Config *config.Config
	UI     cli.Ui
}

// Run runs the daemon command
func (c *Command) Run(args []string) int {
	cmd := getCmd(c.Config, c.UI)
	cmd.SetArgs(args)
	err := cmd.Execute()
	if err != nil {
		return 1
	}
	return 0
}

// Help returns information about the `daemon` command
func (c *Command) Help() string {
	cmd := getCmd(c.Config, c.UI)
	return util.HelpForCobraCmd(cmd)
}

// Synopsis of daemon command
func (c *Command) Synopsis() string {
	cmd := getCmd(c.Config, c.UI)
	return cmd.Short
}

type daemon struct {
	ui         cli.Ui
	logger     hclog.Logger
	repoRoot   fs.AbsolutePath
	timeout    time.Duration
	reqCh      chan struct{}
	timedOutCh chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
}

func getDaemonFileRoot(repoRoot fs.AbsolutePath) fs.AbsolutePath {
	tempDir := fs.GetTempDir("turbod")

	pathHash := sha256.Sum256([]byte(repoRoot.ToString()))
	// We grab a substring of the hash because there is a 108-character limit on the length
	// of a filepath for unix domain socket.
	hexHash := hex.EncodeToString(pathHash[:])[:16]
	return tempDir.Join(hexHash)
}

func getUnixSocket(repoRoot fs.AbsolutePath) fs.AbsolutePath {
	root := getDaemonFileRoot(repoRoot)
	return root.Join("turbod.sock")
}

// logError logs an error and outputs it to the UI.
func (d *daemon) logError(err error) {
	d.logger.Error("error", err)
	d.ui.Error(fmt.Sprintf("%s%s", ui.ERROR_PREFIX, color.RedString(" %v", err)))
}

func getCmd(config *config.Config, ui cli.Ui) *cobra.Command {
	var idleTimeout time.Duration
	cmd := &cobra.Command{
		Use:           "turbo daemon",
		Short:         "Runs turbod",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			d := &daemon{
				ui: ui,
				logger: hclog.New(&hclog.LoggerOptions{
					Output: os.Stdout,
					Level:  hclog.Debug,
					Color:  hclog.AutoColor,
					Name:   "turbod",
				}),
				repoRoot:   config.Cwd,
				timeout:    idleTimeout,
				reqCh:      make(chan struct{}),
				timedOutCh: make(chan struct{}),
				ctx:        ctx,
				cancel:     cancel,
			}
			err := d.runTurboServer()
			if err != nil {
				d.logError(err)
			}
			return err
		},
	}
	cmd.Flags().DurationVar(&idleTimeout, "idle-time", 2*time.Hour, "Set the idle timeout for turbod")
	return cmd
}

var (
	errAlreadyRunning    = errors.New("turbod is already running")
	errInactivityTimeout = errors.New("turbod shut down from inactivity")
)

func (d *daemon) debounceServers(sockPath fs.AbsolutePath) error {
	if !sockPath.FileExists() {
		return nil
	}
	// The socket file exists, can we connect to it?
	return errors.Wrapf(errAlreadyRunning, "socket file already exists at %v", sockPath)
}

func (d *daemon) runTurboServer() error {
	defer d.cancel()

	sockPath := getUnixSocket(d.repoRoot)
	d.logger.Debug(fmt.Sprintf("Using socket path %v (%v)\n", sockPath, len(sockPath)))
	err := d.debounceServers(sockPath)
	if err != nil {
		return err
	}
	err = sockPath.EnsureDir()
	if err != nil {
		return err
	}
	turboServer, err := server.New(d.logger, d.repoRoot)
	if err != nil {
		return err
	}
	defer func() { _ = turboServer.Close() }()
	lis, err := net.Listen("unix", sockPath.ToString())
	if err != nil {
		return err
	}
	// We don't need to explicitly close 'lis', the grpc server will handle that
	s := grpc.NewServer(grpc.UnaryInterceptor(d.onRequest))
	go d.timeoutLoop()

	turboServer.Register(s)
	errCh := make(chan error)
	go func(errCh chan<- error) {
		if err := s.Serve(lis); err != nil {
			errCh <- err
		}
		close(errCh)
	}(errCh)
	var exitErr error
	select {
	case err, ok := <-errCh:
		{
			if ok {
				exitErr = err
			}
			d.cancel()
		}
	case <-d.timedOutCh:
		exitErr = errInactivityTimeout
		s.Stop()
	}
	return exitErr
}

func (d *daemon) onRequest(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	d.reqCh <- struct{}{}
	return handler(ctx, req)
}

func (d *daemon) timeoutLoop() {
	timeoutCh := time.After(d.timeout)
	for {
		select {
		case <-d.reqCh:
			timeoutCh = time.After(d.timeout)
		case <-timeoutCh:
			close(d.timedOutCh)
			break
		case <-d.ctx.Done():
			break
		}
	}
}

type ClientOpts struct{}

type Client struct {
	server.TurboClient
	conn *grpc.ClientConn
}

func (c *Client) Close() error {
	return c.conn.Close()
}

func RunClient(repoRoot fs.AbsolutePath, logger hclog.Logger, opts ClientOpts) (*Client, error) {
	creds := insecure.NewCredentials()

	sockPath, err := getOrStartServer(repoRoot, logger, opts)
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("unix://%v", sockPath.ToString())
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, err
	}
	c := server.NewTurboClient(conn)
	return &Client{
		TurboClient: c,
		conn:        conn,
	}, nil
}

// getOrStartServer looks for an existing socket file, and starts a server if it can't find one
func getOrStartServer(repoRoot fs.AbsolutePath, logger hclog.Logger, opts ClientOpts) (fs.AbsolutePath, error) {
	sockPath := getUnixSocket(repoRoot)
	if sockPath.FileExists() {
		logger.Debug(fmt.Sprintf("found existing turbod socket at %v", sockPath))
		return sockPath, nil
	}
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	logger.Debug(fmt.Sprintf("starting turbod binary %v", bin))
	cmd := exec.Command(bin, "daemon", "--idle-time=15s")
	err = cmd.Start()
	if err != nil {
		return "", err
	}
	for i := 0; i < 150; i++ {
		<-time.After(20 * time.Millisecond)
		if sockPath.FileExists() {
			return sockPath, nil
		}
	}
	return "", errors.New("timed out waiting for turbod to start")
}

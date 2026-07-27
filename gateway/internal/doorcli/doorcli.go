// Package doorcli is the front door entrypoint, invoked as
// `baseten-switch door`: a tiny static
// reverse proxy that owns the harness-facing ports, forwarding to the
// Baseten Switch router process when healthy and to the native upstream when
// tripped. Port specs come from gateway.yaml's door: section by
// default; explicit --port flags override the config file. It absorbs
// the lifecycle contract into the single public executable.
package doorcli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/basetenlabs/baseten-switch/gateway/internal/config"
	"github.com/basetenlabs/baseten-switch/gateway/internal/door"
	"github.com/basetenlabs/baseten-switch/gateway/internal/pidfile"
)

type portList []door.PortSpec

func (p *portList) String() string {
	out := make([]string, 0, len(*p))
	for _, sp := range *p {
		out = append(out, fmt.Sprintf("%d=%s:%s", sp.Port, sp.Shape, sp.RouterTarget))
	}
	return fmt.Sprint(out)
}

func (p *portList) Set(s string) error {
	sp, err := door.ParsePortSpec(s)
	if err != nil {
		return err
	}
	*p = append(*p, sp)
	return nil
}

// Options is the parsed flag surface of `baseten-switch door`: --config,
// --port, --cooldown, --probe-interval, --anthropic-url, and --openai-url.
type Options struct {
	Ports         []door.PortSpec
	ConfigPath    string
	Cooldown      time.Duration
	ProbeInterval time.Duration
	AnthropicURL  string
	OpenAIURL     string
	// Explicit records which flags were set on the command line, for
	// the config-mode "--cooldown ignored" notice.
	Explicit map[string]bool
	usage    func()
}

// ParseFlags parses the door flag surface without any side effects
// beyond writing parse errors and usage to errOut.
func ParseFlags(args []string, errOut io.Writer) (*Options, error) {
	fs := flag.NewFlagSet("baseten-switch door", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: baseten-switch door [--config PATH | --port PORT=shape:routerHost:routerPort ...] [flags]

Without --port flags, port specs come from the gateway.yaml door:
section (--config, else $BASETEN_SWITCH_CONFIG_PATH, else %s)
and SIGHUP re-reads it. Explicit --port flags override the config file
entirely; SIGHUP is then a no-op.

Example (flags mode):
  baseten-switch door \
    --port 45271=anthropic:127.0.0.1:45272

Flags:
`, config.DefaultPath())
		fs.PrintDefaults()
	}
	var ports portList
	fs.Var(&ports, "port", "listener spec PORT=shape:routerHost:routerPort (repeatable; shape: anthropic|openai; overrides --config)")
	configPath := fs.String("config", "", "gateway.yaml with a door: section (default: $BASETEN_SWITCH_CONFIG_PATH, else the standard path)")
	cooldown := fs.Duration("cooldown", door.DefaultCooldown, "after a trip, requests skip the router for this long (flags mode; config mode uses door.cooldown)")
	probeInterval := fs.Duration("probe-interval", door.DefaultProbeInterval, "router /healthz probe interval (flags mode; config mode uses door.probe_interval)")
	anthropicURL := fs.String("anthropic-url", door.DefaultAnthropicBase, "fallback base URL for anthropic-shape ports")
	openaiURL := fs.String("openai-url", door.DefaultOpenAIBase, "fallback base URL for openai-shape ports")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { explicit[fl.Name] = true })
	return &Options{
		Ports:         ports,
		ConfigPath:    *configPath,
		Cooldown:      *cooldown,
		ProbeInterval: *probeInterval,
		AnthropicURL:  *anthropicURL,
		OpenAIURL:     *openaiURL,
		Explicit:      explicit,
		usage:         fs.Usage,
	}, nil
}

// BuildSpecs resolves the parsed options into the door configs to
// serve, plus the config path in config mode ("" in flags mode).
// Notices (mode banners) go to errOut.
func BuildSpecs(opts *Options, errOut io.Writer) (specs []door.Config, cfgPath string, err error) {
	dopts := door.SpecsOptions{AnthropicBase: opts.AnthropicURL, OpenAIBase: opts.OpenAIURL}
	if len(opts.Ports) > 0 {
		fmt.Fprintf(errOut, "[baseten-switch door] --port flags set; gateway.yaml config ignored\n")
		if err := door.ValidatePortSpecs(opts.Ports); err != nil {
			return nil, "", err
		}
		if opts.Cooldown <= 0 {
			return nil, "", fmt.Errorf("--cooldown must be positive")
		}
		if opts.ProbeInterval <= 0 {
			return nil, "", fmt.Errorf("--probe-interval must be positive")
		}
		for _, sp := range opts.Ports {
			base := opts.AnthropicURL
			if sp.Shape == door.ShapeOpenAI {
				base = opts.OpenAIURL
			}
			specs = append(specs, door.Config{
				ListenAddr:    fmt.Sprintf("127.0.0.1:%d", sp.Port),
				Shape:         sp.Shape,
				RouterTarget:  sp.RouterTarget,
				FallbackBase:  base,
				Cooldown:      opts.Cooldown,
				ProbeInterval: opts.ProbeInterval,
			})
		}
		return specs, "", nil
	}
	if opts.Explicit["cooldown"] || opts.Explicit["probe-interval"] {
		fmt.Fprintf(errOut, "[baseten-switch door] --cooldown/--probe-interval ignored in config mode; set door.cooldown / door.probe_interval\n")
	}
	cfgPath = opts.ConfigPath
	if cfgPath == "" {
		cfgPath = os.Getenv("BASETEN_SWITCH_CONFIG_PATH")
	}
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	f, err := config.Load(cfgPath)
	if err != nil {
		return nil, "", err
	}
	specs, err = door.SpecsFromConfig(f, dopts)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %v (add a door: section to the config or pass --port flags)", cfgPath, err)
	}
	fmt.Fprintf(errOut, "[baseten-switch door] config %s: %d door port(s)\n", cfgPath, len(specs))
	return specs, cfgPath, nil
}

// Run is the `baseten-switch door` entrypoint. It serves until SIGTERM or
// SIGINT; SIGHUP re-reads the config in config mode. It writes the
// door pidfile (BASETEN_SWITCH_DOOR_PIDFILE, default ~/.config/baseten-switch/door.pid)
// once every port is bound, so `baseten-switch down` can manage doors
// regardless of who started them.
func Run(args []string) int {
	opts, err := ParseFlags(args, os.Stderr)
	if err != nil {
		return 2
	}
	fail := func(msg string) int {
		fmt.Fprintf(os.Stderr, "baseten-switch door: %s\n\n", msg)
		opts.usage()
		return 2
	}
	specs, cfgPath, err := BuildSpecs(opts, os.Stderr)
	if err != nil {
		return fail(err.Error())
	}
	flagsMode := cfgPath == ""
	dopts := door.SpecsOptions{AnthropicBase: opts.AnthropicURL, OpenAIBase: opts.OpenAIURL}

	serveErr := make(chan error, 16)
	doors := map[string]*door.Door{}
	for _, sp := range specs {
		d, err := startOne(sp, serveErr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "baseten-switch door: %v\n", err)
			shutdownAll(doors)
			return 1
		}
		doors[sp.ListenAddr] = d
	}

	pf := pidfile.DoorPath()
	if err := pidfile.WriteAt(pf, os.Getpid()); err != nil {
		fmt.Fprintf(os.Stderr, "baseten-switch door: write pidfile %s: %v\n", pf, err)
		shutdownAll(doors)
		return 1
	}
	defer pidfile.UnlinkAt(pf)

	sc := make(chan os.Signal, 2)
	signal.Notify(sc, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	for {
		select {
		case sig := <-sc:
			if sig == syscall.SIGHUP {
				if flagsMode {
					fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP ignored: ports came from --port flags, not a config file\n")
					continue
				}
				specs = reload(cfgPath, dopts, specs, doors, serveErr)
				continue
			}
			fmt.Fprintf(os.Stderr, "[baseten-switch door] %s: draining and shutting down\n", sig)
			shutdownAll(doors)
			return 0
		case err := <-serveErr:
			fmt.Fprintf(os.Stderr, "baseten-switch door: %v\n", err)
			return 1
		}
	}
}

// startOne builds, binds and serves a door, reporting Serve failures
// on serveErr.
func startOne(cfg door.Config, serveErr chan<- error) (*door.Door, error) {
	d, err := door.New(cfg)
	if err != nil {
		return nil, err
	}
	if err := d.Start(); err != nil {
		return nil, err
	}
	go func() {
		if err := d.Serve(); err != nil {
			serveErr <- fmt.Errorf("%s: %w", cfg.ListenAddr, err)
		}
	}()
	fmt.Fprintf(os.Stderr, "[baseten-switch door] listening on %s (%s) -> router %s, fallback %s\n",
		d.Addr(), cfg.Shape, cfg.RouterTarget, describeFallback(cfg))
	return d, nil
}

func describeFallback(cfg door.Config) string {
	if len(cfg.Fallbacks) == 0 {
		return cfg.FallbackBase
	}
	out := make([]string, 0, len(cfg.Fallbacks))
	for _, fr := range cfg.Fallbacks {
		out = append(out, fr.Prefix+"->"+fr.Base)
	}
	return strings.Join(out, ", ")
}

// reload re-reads the config on SIGHUP and applies the spec diff:
// unchanged ports keep serving, removed ports shut down, added ports
// bind. Any load error keeps the current ports untouched.
func reload(path string, opts door.SpecsOptions, current []door.Config, doors map[string]*door.Door, serveErr chan<- error) []door.Config {
	f, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: %v; keeping current ports\n", err)
		return current
	}
	next, err := door.SpecsFromConfig(f, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: %s: %v; keeping current ports\n", path, err)
		return current
	}
	added, removed, unchanged := door.DiffSpecs(current, next)
	if len(added) == 0 && len(removed) == 0 {
		fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: config unchanged (%d port(s))\n", len(unchanged))
		return current
	}
	// Removed ports shut down before added ones bind so a changed spec
	// can rebind its address.
	for _, sp := range removed {
		d := doors[sp.ListenAddr]
		if d == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = d.Shutdown(ctx)
		cancel()
		delete(doors, sp.ListenAddr)
		fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: stopped %s (%s)\n", sp.ListenAddr, sp.Shape)
	}
	result := unchanged
	for _, sp := range added {
		d, err := startOne(sp, serveErr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: start %s failed: %v\n", sp.ListenAddr, err)
			continue
		}
		doors[sp.ListenAddr] = d
		result = append(result, sp)
	}
	fmt.Fprintf(os.Stderr, "[baseten-switch door] SIGHUP: %d unchanged, %d added, %d removed\n",
		len(unchanged), len(added), len(removed))
	return result
}

func shutdownAll(doors map[string]*door.Door) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, d := range doors {
		_ = d.Shutdown(ctx)
	}
}

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"sync"
	"syscall"

	"github.com/gliderlabs/ssh"
	"github.com/miekg/gitopper/ospkg"
	"github.com/miekg/gitopper/osutil"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.science.ru.nl/log"
)

type ExecContext struct {
	// Configuration
	Hosts        []string
	ConfigSource string
	SAddr        string
	MAddr        string
	Debug        bool
	Restart      bool
	Upstream     string
	Dir          string
	Branch       string
	Mount        string
	Pull         bool

	// Runtime State
	HTTPMux *http.ServeMux
}

func (exec *ExecContext) RegisterFlags(fs *flag.FlagSet) {
	if fs == nil {
		fs = flag.CommandLine
	}
	fs.Var(&sliceFlag{&exec.Hosts}, "h", "hosts (comma separated) to impersonate, local hostname is included by default")
	fs.StringVar(&exec.ConfigSource, "c", "", "config file to read")
	fs.StringVar(&exec.SAddr, "s", ":2222", "ssh address to listen on")
	fs.StringVar(&exec.MAddr, "m", ":9222", "http metrics address to listen on")
	fs.BoolVar(&exec.Debug, "d", false, "enable debug logging")
	fs.BoolVar(&exec.Restart, "r", false, "send SIGHUP when config changes")

	// bootstrap flags
	fs.StringVar(&exec.Upstream, "U", "", "[bootstrapping] use this git repo")
	fs.StringVar(&exec.Dir, "D", "gitopper", "[bootstrapping] directory to sparse checkout")
	fs.StringVar(&exec.Branch, "B", "main", "[bootstrapping] check out in this branch")
	fs.StringVar(&exec.Mount, "M", "", "[bootstrapping] check out into this directory, -c is relative to this dir")
	fs.BoolVar(&exec.Pull, "P", false, "[boostrapping] pull (update) the git repo to the newest version before starting")
}

var (
	ErrNotRoot  = errors.New("not root")
	ErrNoConfig = errors.New("-c flag is mandatory")
	ErrHUP      = errors.New("hangup requested")
)

type RepoPullError struct {
	Machine    string
	Upstream   string
	Underlying error
}

func (err *RepoPullError) Error() string {
	return fmt.Sprintf("Machine %q, error pulling repo %q: %s", err.Machine, err.Upstream, err.Underlying)
}

func (err *RepoPullError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Underlying
}

func serveMonitoring(exec *ExecContext, controllerWG, workerWG *sync.WaitGroup) error {
	exec.HTTPMux.Handle("/metrics", promhttp.Handler())
	ln, err := net.Listen("tcp", exec.MAddr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:    exec.MAddr,
		Handler: exec.HTTPMux,
	}
	controllerWG.Add(1) // Ensure  HTTP server draining blocks application shutdown.
	go func() {
		defer controllerWG.Done()
		workerWG.Wait()              // Unblocks upon context cancellation and workers finishing.
		srv.Shutdown(context.TODO()) // TODO: Derive context tree more carefully from root.
	}()
	controllerWG.Add(1)
	go func() {
		defer controllerWG.Done()
		err := srv.Serve(ln)
		switch {
		case err == nil:
		case errors.Is(err, http.ErrServerClosed):
		default:
			log.Fatal(err)
		}
	}()
	return nil
}

func serveSSH(exec *ExecContext, controllerWG, workerWG *sync.WaitGroup, allowed []ssh.PublicKey, sshHandler ssh.Handler) error {
	l, err := net.Listen("tcp", exec.SAddr)
	if err != nil {
		return err
	}
	srv := &ssh.Server{Addr: exec.SAddr, Handler: sshHandler}
	srv.SetOption(ssh.PublicKeyAuth(func(ctx ssh.Context, key ssh.PublicKey) bool {
		for _, a := range allowed {
			if ssh.KeysEqual(a, key) {
				return true
			}
		}
		return false
	}))
	controllerWG.Add(1) // Ensure SSH server draining blocks application shutdown.
	go func() {
		defer controllerWG.Done()
		workerWG.Wait()              // Unblocks upon context cancellation and workers finishing.
		srv.Shutdown(context.TODO()) // TODO: Derive context tree more carefully from root.
	}()
	controllerWG.Add(1)
	go func() {
		defer controllerWG.Done()
		err := srv.Serve(l)
		switch {
		case err == nil:
		case errors.Is(err, ssh.ErrServerClosed):
		default:
			log.Fatal(err)
		}
	}()
	return nil
}

func run(exec *ExecContext) error {
	if os.Geteuid() != 0 {
		return ErrNotRoot
	}

	if exec.Debug {
		log.D.Set()
	}

	if exec.ConfigSource == "" {
		return ErrNoConfig
	}

	// bootstrapping
	self := selfService(exec.Upstream, exec.Branch, exec.Mount, exec.Dir)
	if self != nil {
		log.Infof("Bootstapping from repo %q and adding service %q for %q", exec.Upstream, self.Service, self.Machine)
		gc := self.newGitCmd()
		err := gc.Checkout()
		if err != nil {
			return &RepoPullError{self.Machine, self.Upstream, err}
		}
		if exec.Pull {
			if _, err := gc.Pull(); err != nil {
				return &RepoPullError{self.Machine, self.Upstream, err}
			}
		}
		exec.ConfigSource = path.Join(path.Join(path.Join(self.Mount, self.Service), exec.Dir), exec.ConfigSource)
		log.Infof("Setting config to %s", exec.ConfigSource)
	}

	doc, err := os.ReadFile(exec.ConfigSource)
	if err != nil {
		return fmt.Errorf("reading config: %v", err)
	}
	c, err := parseConfig(doc)
	if err != nil {
		return fmt.Errorf("parsing config: %v", err)
	}

	if err := c.Valid(); err != nil {
		return fmt.Errorf("validating config: %v", err)
	}

	if self != nil {
		c.Services = append(c.Services, self)
	}

	allowed := make([]ssh.PublicKey, len(c.Keys.Path))
	for i, p := range c.Keys.Path {
		if !path.IsAbs(p) && self != nil { // bootstrapping
			newpath := path.Join(path.Join(path.Join(self.Mount, self.Service), exec.Dir), p)
			p = newpath
		}

		log.Infof("Reading public key %q", p)
		data, err := ioutil.ReadFile(p)
		if err != nil {
			return err
		}
		a, _, _, _, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return err
		}
		allowed[i] = a
	}

	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	sshHandler := newRouter(c, exec.Hosts)
	var workerWG, controllerWG sync.WaitGroup
	defer controllerWG.Wait()
	if err := serveSSH(exec, &controllerWG, &workerWG, allowed, sshHandler); err != nil {
		return err
	}
	if err := serveMonitoring(exec, &controllerWG, &workerWG); err != nil {
		return err
	}
	log.Infof("Launched servers on port %s (ssh) and %s (metrics) for machines: %v, %d public keys loaded", exec.SAddr, exec.MAddr, exec.Hosts, len(c.Keys.Path))
	pkg := ospkg.New()
	servCnt := 0
	for _, serv := range c.Services {
		if !serv.forMe(exec.Hosts) {
			continue
		}

		servCnt++
		s := serv.merge(c.Global)
		log.Infof("Machine %q %q", s.Machine, s.Upstream)
		gc := s.newGitCmd()

		if s.Package != "" {
			if err := pkg.Install(s.Package); err != nil {
				log.Warningf("Machine %q, error installing package %q: %s", s.Machine, s.Package, err)
				continue // skip this, or continue, if continue and with the bind mounts the future pkg install might also break...
				// or fatal error??
			}
		}

		// Initial checkout - if needed.
		err := gc.Checkout()
		if err != nil {
			log.Warningf("Machine %q, error pulling repo %q: %s", s.Machine, s.Upstream, err)
			s.SetState(StateBroken, fmt.Sprintf("error pulling %q: %s", s.Upstream, err))
			continue
		}

		log.Infof("Machine %q, repository in %q with %q", s.Machine, gc.Repo(), gc.Hash())

		// all succesfully done, do the bind mounts and start our puller
		mounts, err := s.bindmount()
		if err != nil {
			log.Warningf("Machine %q, error setting up bind mounts for %q: %s", s.Machine, s.Upstream, err)
			s.SetState(StateBroken, fmt.Sprintf("error setting up bind mounts repo %q: %s", s.Upstream, err))
			continue
		}
		// Restart any services as they see new files in their bindmounts. Do this here, because we can't be
		// sure there is an update to a newer commit that would also kick off a restart.
		if mounts > 0 {
			if err := s.systemctl(); err != nil {
				log.Warningf("Machine %q, error running systemctl: %s", s.Machine, err)
				s.SetState(StateBroken, fmt.Sprintf("error running systemctl %q: %s", s.Upstream, err))
				// no continue; maybe git pull will make this work later
			}
		}

		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			s.trackUpstream(ctx)
		}()
	}

	if servCnt == 0 {
		log.Warningf("No services found for machine: %v, exiting", exec.Hosts)
		return nil
	}

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	if exec.Restart {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			trackConfig(ctx, exec.ConfigSource, done)
		}()
	}
	hup := make(chan struct{})
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		select {
		case s := <-done:
			cancel()
			if s == syscall.SIGHUP {
				close(hup)
			}
		case <-ctx.Done():
		}
	}()
	workerWG.Wait()
	select {
	case <-hup:
		return ErrHUP
	default:
	}
	return nil
}

func main() {
	exec := ExecContext{
		Hosts:   []string{osutil.Hostname()},
		HTTPMux: http.NewServeMux(),
	}
	exec.RegisterFlags(nil)

	flag.Parse()
	err := run(&exec)
	switch {
	case err == nil:
	case errors.Is(err, ErrHUP):
		// on HUP exit with exit status 2, so systemd can restart us (Restart=OnFailure)
		os.Exit(2)
	default:
		log.Fatal(err)
	}
}

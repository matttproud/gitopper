package main

import (
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	toml "github.com/pelletier/go-toml/v2"
	"go.science.ru.nl/log"
)

var (
	flagConfig   = flag.String("c", "", "config file to read")
	flagDuration = flag.String("d", "30s", "default duration to sleep before pulling")
	flagAddr     = flag.String("a", ":8000", "address to listen on")
)

func main() {
	flag.Parse()

	if *flagConfig == "" {
		log.Fatalf("-c flag is mandatory")
	}
	duration, err := time.ParseDuration(*flagDuration)
	if err != nil {
		log.Fatalf("invalid -d value: %s", err)
	}
	if duration < 5*time.Second {
		log.Fatal("invalid -d value (<5s)")
	}

	doc, err := os.ReadFile(*flagConfig)
	if err != nil {
		log.Fatal(err)
	}

	var c Config
	if err := toml.Unmarshal([]byte(doc), &c); err != nil {
		log.Fatal(err)
	}

	if err := c.Valid(); err != nil {
		log.Fatalf("The configuration is not valid: %s", err)
	}

	for _, s := range c.Services {
		s1 := s.merge(c.Global, duration)

		log.Infof("Machine %q %q", s1.Machine, s1.Upstream)
		gc := s1.newGitCmd()

		// Initial checkout - if needed.
		err := gc.Checkout()
		if err != nil {
			log.Warningf("Machine %q, error checking out: %s", s1.Machine, err)
			// state change broken
			// continue?? - yes continue
		}
		log.Infof("Machine %q, repository in %q with %q", s1.Machine, gc.Repo(), gc.Hash())

		// all succesfully done, do the bind mounts and start our puller
		if err := s1.bindmount(); err != nil {
			// state change
			log.Fatalf("Can not setup bind mounts: %s", err)
		}
		go s1.trackUpstream(nil) // TODO: stop goroutines, could also use context.
	}

	router := newRouter(c)

	go func() {
		if err := http.ListenAndServe(*flagAddr, router); err != nil {
			log.Fatal(err)
		}
	}()
	log.Infof("Launched server on port %s", *flagAddr)

	done := make(chan os.Signal, 1)
	signal.Notify(done, syscall.SIGINT, syscall.SIGTERM)
	<-done
}

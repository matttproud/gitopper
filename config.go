package main

// Config holds the gitopper config file. It's is updated every so often to pick up new changes.
type Config struct {
	Global   Service // although not _all_ fields
	Services []Service
}

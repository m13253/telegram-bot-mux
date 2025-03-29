package main

import (
	"context"
	"flag"
	"log"
)

func main() {
	confPath := flag.String("conf", "tbmux.conf", "Configuration file")
	flag.Parse()

	conf, err := Load(*confPath)
	if err != nil {
		log.Fatalln(err)
	}
	db, err := OpenDatabase(conf)
	if err != nil {
		log.Fatalln(err)
	}
	c := NewClient(conf, db)
	s, err := NewServer(conf, db, c)
	if err != nil {
		log.Fatalln(err)
	}

	go func() {
		err := s.Serve()
		if err != nil {
			log.Fatalln(err)
		}
	}()

	err = c.StartPolling(context.Background())
	log.Fatalln(err)
}

// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	log "github.com/golang/glog"

	"github.com/juju/errors"
	"github.com/jvaidya/mysql-vt-proxy/server"
)

var (
	host         = flag.String("host", "0.0.0.0", "Mysql Vitess proxy server host")
	vtgate       = flag.String("vtgate", "localhost:15991", "Vitess vtgate host:port to connect to.")
	keyspace     = flag.String("keyspace", "", "Vitess keyspace to use.")
	tabletType   = flag.String("tablettype", "master", "Vitess tabletType to use for the keyspace.")
	port         = flag.String("P", "15891", "Mysql Vitess proxy server port")
	statusPort   = flag.String("status", "10080", "Mysql Vitess proxy server status port")
	reportStatus = flag.Bool("report-status", true, "If enabled, report status via HTTP service.")
)

func main() {

	runtime.GOMAXPROCS(runtime.NumCPU())

	flag.Parse()

	cfg := &server.Config{
		Addr:         fmt.Sprintf("%s:%s", *host, *port),
		StatusAddr:   fmt.Sprintf(":%s", *statusPort),
		Socket:       "",
		ReportStatus: *reportStatus,
		VtAddress:    *vtgate,
		VtTabletType: *tabletType,
		VtKeyspace:   *keyspace,
	}
	flag.Set("alsologtostderr", "true")
	flag.Set("v", "5")

	log.Infof("Welcome to Mysql Vitess proxy.")
	log.Infof("Will use vtgate at %v for %v:%v", cfg.VtAddress, cfg.VtKeyspace, cfg.VtTabletType)

	svr, err := server.NewServer(cfg)
	if err != nil {
		log.Errorf(errors.ErrorStack(err))
		os.Exit(1)
	}

	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	go func() {
		sig := <-sc
		log.Infof("Got signal [%d] to exit.", sig)
		svr.Close()
		//os.Exit(0)
	}()

	err = svr.Run()

	if err != nil {
		log.Errorf(err.Error())
	}
}

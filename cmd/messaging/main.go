// Copyright 2016-2018 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"time"

	tskdb "../../internal/task"
	"github.com/nats-io/go-nats-streaming"

	"../../internal/task-api"
	"github.com/nats-io/go-nats-streaming/pb"
	"upper.io/db.v3/sqlite"
)

var usageStr = `
Usage: stan-sub [options] <subject>

Options:
	-s, --server   <url>            NATS Streaming server URL(s)
	-c, --cluster  <cluster name>   NATS Streaming cluster name
	-id,--clientid <client ID>      NATS Streaming client ID

Subscription Options:
	--qgroup <name>                 Queue group
	--seq <seqno>                   Start at seqno
	--all                           Deliver all available messages
	--last                          Deliver starting with last published message
	--since <duration>              Deliver messages in last interval (e.g. 1s, 1hr)
	         (for more information: https://golang.org/pkg/time/#ParseDuration)
	--durable <name>                Durable subscriber name
	--unsubscribe                   Unsubscribe the durable on exit
`

// NOTE: Use tls scheme for TLS, e.g. stan-sub -s tls://demo.nats.io:4443 foo
func usage() {
	log.Fatalf(usageStr)
}

func printMsg(m *stan.Msg, i int) {
	log.Printf("[#%d] Received on [%s]: '%s'\n", i, m.Subject, m)
}

func main() {

	// ConnectionURL implements a SQLite connection struct.
	type ConnectionURL struct {
		Database string
		Options  map[string]string
	}

	var settings = sqlite.ConnectionURL{
		Database: `data/task.db`, // Path to database file.
	}

	dbSession, err := sqlite.Open(settings)
	if err != nil {

		log.Fatalf("db.Open(): %q\n", err)
	}
	defer dbSession.Close()
	// DB session weitergeben
	tskdb.ConnectDatabase(dbSession)

	// grpc handler
	handler := task.GetServiceServer()

	var clusterID string
	var clientID string
	var showTime bool
	var startSeq uint64
	var startDelta string
	var deliverAll bool
	var deliverLast bool
	var durable string
	var qgroup string
	var unsubscribe bool
	var URL string

	//	defaultID := fmt.Sprintf("client.%s", nuid.Next())

	flag.StringVar(&URL, "s", stan.DefaultNatsURL, "The nats server URLs (separated by comma)")
	flag.StringVar(&URL, "server", stan.DefaultNatsURL, "The nats server URLs (separated by comma)")
	flag.StringVar(&clusterID, "c", "test-cluster", "The NATS Streaming cluster ID")
	flag.StringVar(&clusterID, "cluster", "test-cluster", "The NATS Streaming cluster ID")
	flag.StringVar(&clientID, "id", "task_engine", "The NATS Streaming client ID to connect with")
	flag.StringVar(&clientID, "clientid", "task_engine", "The NATS Streaming client ID to connect with")
	flag.BoolVar(&showTime, "t", true, "Display timestamps")
	// Subscription options
	flag.Uint64Var(&startSeq, "seq", 0, "Start at sequence no.")
	flag.BoolVar(&deliverAll, "all", false, "Deliver all")
	flag.BoolVar(&deliverLast, "last", false, "Start with last value")
	flag.StringVar(&startDelta, "since", "", "Deliver messages since specified time offset")
	flag.StringVar(&durable, "durable", "taskServer", "Durable subscriber name")
	flag.StringVar(&qgroup, "qgroup", "", "Queue group name")
	flag.BoolVar(&unsubscribe, "unsubscribe", false, "Unsubscribe the durable on exit")

	log.SetFlags(0)
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()

	if clientID == "" {
		log.Printf("Error: A unique client ID must be specified.")
		usage()
	}
	if len(args) < 1 {
		log.Printf("Error: A subject must be specified.")
		usage()
	}

	sc, err := stan.Connect(clusterID, clientID, stan.NatsURL(URL),
		stan.SetConnectionLostHandler(func(_ stan.Conn, reason error) {
			log.Fatalf("Connection lost, reason: %v", reason)
		}))
	if err != nil {
		log.Fatalf("Can't connect: %v.\nMake sure a NATS Streaming Server is running at: %s", err, URL)
	}
	log.Printf("Connected to %s clusterID: [%s] clientID: [%s]\n", URL, clusterID, clientID)

	subj, i := args[0], 0

	callbackfuncs := map[string]func(msg *stan.Msg){}

	// callback function für empfangene nachricht
	callbackfuncs["createTask"] = func(msg *stan.Msg) {
		t := task.CreateTaskRequest{}
		err := t.Unmarshal(msg.Data)
		if err != nil {
			log.Println(msg)
		} else {
			handler.CreateTask(context.Background(), &t)
		}
		i++
		printMsg(msg, i)

	}

	startOpt := stan.StartAt(pb.StartPosition_NewOnly)

	if startSeq != 0 {
		startOpt = stan.StartAtSequence(startSeq)
	} else if deliverLast {
		startOpt = stan.StartWithLastReceived()
	} else if deliverAll {
		log.Print("subscribing with DeliverAllAvailable")
		startOpt = stan.DeliverAllAvailable()
	} else if startDelta != "" {
		ago, err := time.ParseDuration(startDelta)
		if err != nil {
			sc.Close()
			log.Fatal(err)
		}
		startOpt = stan.StartAtTimeDelta(ago)
	}

	sub, err := sc.QueueSubscribe(subj, qgroup, callbackfuncs["createTask"], startOpt, stan.DurableName(durable))
	if err != nil {
		sc.Close()
		log.Fatal(err)
	}

	log.Printf("Listening on [%s], clientID=[%s], qgroup=[%s] durable=[%s]\n", subj, clientID, qgroup, durable)

	if showTime {
		log.SetFlags(log.LstdFlags)
	}

	// Wait for a SIGINT (perhaps triggered by user with CTRL-C)
	// Run cleanup when signal is received
	signalChan := make(chan os.Signal, 1)
	cleanupDone := make(chan bool)
	signal.Notify(signalChan, os.Interrupt)
	go func() {
		for range signalChan {
			fmt.Printf("\nReceived an interrupt, unsubscribing and closing connection...\n\n")
			// Do not unsubscribe a durable on exit, except if asked to.
			if durable == "" || unsubscribe {
				sub.Unsubscribe()
			}
			sc.Close()
			cleanupDone <- true
		}
	}()
	<-cleanupDone
}

// Copyright 2019 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"

	"github.com/ethereum/go-ethereum/p2p/discover"
	"gopkg.in/urfave/cli.v1"
)

var (
	discv5Command = cli.Command{
		Name:  "discv5",
		Usage: "Node Discovery v5 tools",
		Subcommands: []cli.Command{
			discv5PingCommand,
			discv5ResolveCommand,
			discv5ListenCommand,
		},
	}
	discv5PingCommand = cli.Command{
		Name:   "ping",
		Usage:  "Sends ping to a node",
		Action: discv5Ping,
	}
	discv5ResolveCommand = cli.Command{
		Name:   "resolve",
		Usage:  "Finds a node in the DHT",
		Action: discv5Resolve,
		Flags:  []cli.Flag{bootnodesFlag},
	}
	discv5ListenCommand = cli.Command{
		Name:   "listen",
		Usage:  "Runs a node",
		Action: discv5Listen,
		Flags:  []cli.Flag{bootnodesFlag},
	}
)

func discv5Ping(ctx *cli.Context) error {
	n := getNodeArg(ctx)
	disc := startV5(nil)
	defer disc.Close()

	fmt.Println(disc.Ping(n))
	return nil
}

func discv5Resolve(ctx *cli.Context) error {
	n := getNodeArg(ctx)
	disc := startV5(nil)
	defer disc.Close()

	fmt.Println(disc.Resolve(n))
	return nil
}

func discv5Listen(ctx *cli.Context) error {
	disc := startV5(nil)
	defer disc.Close()

	fmt.Println(disc.Self())
	select {}
}

// startV5 starts an ephemeral discovery v5 node.
func startV5(ctx *cli.Context) *discover.UDPv5 {
	socket, ln, cfg, err := listen()
	if err != nil {
		exit(err)
	}
	disc, err := discover.ListenV5(socket, ln, cfg)
	if err != nil {
		exit(err)
	}
	return disc
}

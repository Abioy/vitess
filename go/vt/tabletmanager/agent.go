// Copyright 2012, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
The agent listens on a zk node for new actions to perform.

It passes them off to a separate action process. Even though some
actions could be completed inline very quickly, the external process
makes it easy to track and interrupt complex actions that may wedge
due to external circumstances.
*/

package tabletmanager

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/naming"
	"code.google.com/p/vitess/go/zk"
	"launchpad.net/gozk/zookeeper"
)

type ActionAgent struct {
	zconn           zk.Conn
	zkTabletPath    string // FIXME(msolomon) use tabletInfo
	zkActionPath    string
	vtActionBinPath string // path to vt_action binary
	MycnfPath       string // path to my.cnf file

	mutex   sync.Mutex
	_tablet *TabletInfo // must be accessed with lock - TabletInfo objects are not synchronized.
}

// bindAddr: the address for the query service advertised by this agent
func NewActionAgent(zconn zk.Conn, zkTabletPath string) *ActionAgent {
	actionPath := TabletActionPath(zkTabletPath)
	return &ActionAgent{zconn: zconn, zkTabletPath: zkTabletPath, zkActionPath: actionPath}
}

func (agent *ActionAgent) readTablet() error {
	// Reread in case there were changes
	tablet, err := ReadTablet(agent.zconn, agent.zkTabletPath)
	if err != nil {
		return err
	}
	agent.mutex.Lock()
	agent._tablet = tablet
	agent.mutex.Unlock()
	return nil
}

func (agent *ActionAgent) Tablet() *TabletInfo {
	agent.mutex.Lock()
	tablet := agent._tablet
	agent.mutex.Unlock()
	return tablet
}

// FIXME(msolomon) need a real path discovery mechanism, a config file
// or more command line args.
func (agent *ActionAgent) resolvePaths() error {
	vtActionBinPaths := []string{os.ExpandEnv("$VTROOT/src/code.google.com/p/vitess/go/cmd/vtaction/vtaction"),
		"/usr/local/bin/vtaction"}
	for _, path := range vtActionBinPaths {
		if _, err := os.Stat(path); err == nil {
			agent.vtActionBinPath = path
			break
		}
	}
	if agent.vtActionBinPath == "" {
		return errors.New("no vtaction binary found")
	}

	mycnfPaths := []string{fmt.Sprintf("/vt/vt_%010d/my.cnf", agent.Tablet().Uid),
		"/var/lib/mysql/my.cnf", "/etc/my.cnf"}
	for _, path := range mycnfPaths {
		if _, err := os.Stat(path); err == nil {
			agent.MycnfPath = path
			break
		}
	}
	if agent.MycnfPath == "" {
		return errors.New("no my.cnf found")
	}
	return nil
}

func (agent *ActionAgent) dispatchAction(actionPath string) {
	relog.Info("action dispatch %v", actionPath)
	data, _, err := agent.zconn.Get(actionPath)
	if err != nil {
		relog.Error("action dispatch failed: %v", err)
		return
	}

	actionNode, err := ActionNodeFromJson(data, actionPath)
	if err != nil {
		relog.Error("action decode failed: %v %v", actionPath, err)
		return
	}

	cmd := []string{
		agent.vtActionBinPath,
		"-action", actionNode.Action,
		"-action-node", actionPath,
		"-action-guid", actionNode.ActionGuid,
		"-mycnf-path", agent.MycnfPath,
		"-logfile", flag.Lookup("logfile").Value.String(),
	}
	relog.Info("action launch %v", cmd)
	vtActionCmd := exec.Command(cmd[0], cmd[1:]...)

	stdOut, vtActionErr := vtActionCmd.CombinedOutput()
	if vtActionErr != nil {
		relog.Error("action failed: %v %v\n%s", actionPath, vtActionErr, stdOut)
		return
	}

	relog.Info("action completed %v %s", actionPath, stdOut)

	// Actions should have side effects on the tablet, so reload the data.
	if err := agent.readTablet(); err != nil {
		relog.Warning("failed rereading tablet after action: %v %v", actionPath, err)
	}
}

func (agent *ActionAgent) handleActionQueue() (<-chan zookeeper.Event, error) {
	// This read may seem a bit pedantic, but it makes it easier for the system
	// to trend towards consistency if an action fails or somehow the action
	// queue gets mangled by an errant process.
	children, _, watch, err := agent.zconn.ChildrenW(agent.zkActionPath)
	if err != nil {
		return watch, err
	}
	if len(children) > 0 {
		sort.Strings(children)
		for _, child := range children {
			actionPath := agent.zkActionPath + "/" + child
			agent.dispatchAction(actionPath)
		}
	}
	return watch, nil
}

func (agent *ActionAgent) verifyZkPaths() error {
	tablet := agent.Tablet()
	if tablet == nil {
		panic(fmt.Errorf("agent._tablet is nil"))
	}

	zkReplicationPath := tablet.ReplicationPath()

	_, err := agent.zconn.Create(zkReplicationPath, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil && err.(*zookeeper.Error).Code != zookeeper.ZNODEEXISTS {
		return err
	}

	// Ensure that the action node is there.
	_, err = agent.zconn.Create(agent.zkActionPath, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil && err.(*zookeeper.Error).Code != zookeeper.ZNODEEXISTS {
		return err
	}
	return nil
}

func (agent *ActionAgent) verifyZkServingAddrs() error {
	if !agent.Tablet().IsServingType() {
		return nil
	}
	// Load the shard and see if we are supposed to be serving. We might be a serving type,
	// but we might be in a transitional state. Only once the shard info is updated do we
	// put ourselves in the client serving graph.
	shardInfo, err := ReadShard(agent.zconn, agent.Tablet().ShardPath())
	if err != nil {
		return err
	}

	if !shardInfo.Contains(agent.Tablet().Tablet) {
		return nil
	}
	// Check to see our address is registered in the right place.
	zkPathName := naming.ZkPathForVtName(agent.Tablet().Tablet.Cell, agent.Tablet().Keyspace,
		agent.Tablet().Shard, string(agent.Tablet().Type))

	f := func(oldValue string, oldStat *zookeeper.Stat) (string, error) {
		return agent.updateEndpoints(oldValue, oldStat)
	}
	err = agent.zconn.RetryChange(zkPathName, 0, zookeeper.WorldACL(zookeeper.PERM_ALL), f)
	if err == skipUpdateErr {
		err = nil
		relog.Warning("skipped serving graph update")
	}
	return err
}

var skipUpdateErr = fmt.Errorf("skip update")

// A function conforming to the RetryChange protocl. If the data returned
// is identical, no update is performed.
func (agent *ActionAgent) updateEndpoints(oldValue string, oldStat *zookeeper.Stat) (newValue string, err error) {
	if oldStat == nil {
		// The incoming object doesn't exist - we haven't been placed in the serving
		// graph yet, so don't update. Assume the next process that rebuilds the graph
		// will get the updated tablet location.
		return "", skipUpdateErr
	}

	addrs := naming.NewAddrs()
	if oldValue != "" {
		err = json.Unmarshal([]byte(oldValue), addrs)
		if err != nil {
			return
		}

		foundTablet := false
		for _, entry := range addrs.Entries {
			if entry.Uid == agent.Tablet().Uid {
				foundTablet = true
				vtAddr := fmt.Sprintf("%v:%v", entry.Host, entry.NamedPortMap["_vtocc"])
				mysqlAddr := fmt.Sprintf("%v:%v", entry.Host, entry.NamedPortMap["_mysql"])
				if vtAddr != agent.Tablet().Addr || mysqlAddr != agent.Tablet().MysqlAddr {
					// update needed
					host, port := splitHostPort(agent.Tablet().Addr)
					entry.Host = host
					entry.NamedPortMap["_vtocc"] = port
					host, port = splitHostPort(agent.Tablet().MysqlAddr)
					entry.NamedPortMap["_mysql"] = port
				}
				break
			}
		}

		if !foundTablet {
			addrs.Entries = append(addrs.Entries, *vtnsAddrForTablet(agent.Tablet().Tablet))
		}
	} else {
		addrs.Entries = append(addrs.Entries, *vtnsAddrForTablet(agent.Tablet().Tablet))
	}
	return toJson(addrs), nil
}

func splitHostPort(addr string) (string, int) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		panic(err)
	}
	p, err := strconv.ParseInt(port, 10, 16)
	if err != nil {
		panic(err)
	}
	return host, int(p)
}

// Resolve an address where the host has been left blank, like ":3306"
func resolveAddr(addr string) string {
	host, port := splitHostPort(addr)
	if host == "" {
		hostname, err := os.Hostname()
		if err != nil {
			panic(err)
		}
		host = hostname
	}
	return fmt.Sprintf("%v:%v", host, port)
}

func vtnsAddrForTablet(tablet *Tablet) *naming.VtnsAddr {
	host, port := splitHostPort(tablet.Addr)
	entry := naming.NewAddr(tablet.Uid, host, 0)
	entry.NamedPortMap["_vtocc"] = port
	host, port = splitHostPort(tablet.MysqlAddr)
	entry.NamedPortMap["_mysql"] = port
	return entry
}

func (agent *ActionAgent) Start(bindAddr, mysqlAddr string) {
	var err error
	if err = agent.readTablet(); err != nil {
		panic(err)
	}

	if err = agent.resolvePaths(); err != nil {
		panic(err)
	}

	// Update bind addr for mysql and query service in the tablet node.
	f := func(oldValue string, oldStat *zookeeper.Stat) (string, error) {
		if oldValue == "" {
			return "", fmt.Errorf("no data for tablet addr update: %v", agent.zkTabletPath)
		}

		tablet := tabletFromJson(oldValue)
		tablet.Addr = resolveAddr(bindAddr)
		tablet.MysqlAddr = resolveAddr(mysqlAddr)
		return toJson(tablet), nil
	}
	err = agent.zconn.RetryChange(agent.Tablet().Path(), 0, zookeeper.WorldACL(zookeeper.PERM_ALL), f)
	if err != nil {
		panic(err)
	}

	// Reread in case there were changes
	if err := agent.readTablet(); err != nil {
		panic(err)
	}

	if err := zk.CreatePidNode(agent.zconn, agent.Tablet().PidPath()); err != nil {
		panic(err)
	}

	if err = agent.verifyZkPaths(); err != nil {
		panic(err)
	}

	if err = agent.verifyZkServingAddrs(); err != nil {
		panic(err)
	}

	go agent.actionEventLoop()
}

func (agent *ActionAgent) actionEventLoop() {
	for {
		// Process any pending actions when we startup, before we start listening
		// for events.
		watch, err := agent.handleActionQueue()
		if err != nil {
			relog.Warning("action queue failed: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}

		event := <-watch
		if !event.Ok() {
			// NOTE(msolomon) The zk meta conn will reconnect automatically, or
			// error out. At this point, there isn't much to do.
			relog.Warning("zookeeper not OK: %v", event)
			time.Sleep(5 * time.Second)
		} else if event.Type == zookeeper.EVENT_CHILD {
			agent.handleActionQueue()
		}
	}
}
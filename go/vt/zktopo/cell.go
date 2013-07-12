// Copyright 2013, Google Inc. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package zktopo

import (
	"fmt"
	"math/rand"
	"path"
	"sort"
	"strings"
	"time"

	"code.google.com/p/vitess/go/jscfg"
	"code.google.com/p/vitess/go/relog"
	"code.google.com/p/vitess/go/vt/naming"
	"code.google.com/p/vitess/go/zk"
	"launchpad.net/gozk/zookeeper"
)

/*
This file contains the per-cell methods of ZkTopologyServer
*/

func tabletPathForAlias(alias naming.TabletAlias) string {
	return fmt.Sprintf("/zk/%v/vt/tablets/%v", alias.Cell, alias.TabletUidStr())
}

func tabletActionPathForAlias(alias naming.TabletAlias) string {
	return fmt.Sprintf("/zk/%v/vt/tablets/%v/action", alias.Cell, alias.TabletUidStr())
}

func tabletDirectoryForCell(cell string) string {
	return fmt.Sprintf("/zk/%v/vt/tablets", cell)
}

//
// Tablet management
//

func (zkts *ZkTopologyServer) CreateTablet(alias naming.TabletAlias, contents string) error {
	zkTabletPath := tabletPathForAlias(alias)

	// Create /zk/<cell>/vt/tablets/<uid>
	_, err := zk.CreateRecursive(zkts.zconn, zkTabletPath, contents, 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNODEEXISTS) {
			err = naming.ErrNodeExists
		}
		return err
	}

	// Create /zk/<cell>/vt/tablets/<uid>/action
	tap := path.Join(zkTabletPath, "action")
	_, err = zkts.zconn.Create(tap, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		return err
	}

	// Create /zk/<cell>/vt/tablets/<uid>/actionlog
	talp := path.Join(zkTabletPath, "actionlog")
	_, err = zkts.zconn.Create(talp, "", 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		return err
	}

	return nil
}

func (zkts *ZkTopologyServer) UpdateTablet(alias naming.TabletAlias, contents string, existingVersion int) (int, error) {
	zkTabletPath := tabletPathForAlias(alias)
	stat, err := zkts.zconn.Set(zkTabletPath, contents, existingVersion)
	if err != nil {
		return 0, err
	}
	return stat.Version(), nil
}

func (zkts *ZkTopologyServer) DeleteTablet(alias naming.TabletAlias) error {
	zkTabletPath := tabletPathForAlias(alias)
	return zk.DeleteRecursive(zkts.zconn, zkTabletPath, -1)
}

func (zkts *ZkTopologyServer) ValidateTablet(alias naming.TabletAlias) error {
	zkTabletPath := tabletPathForAlias(alias)
	zkPaths := []string{
		path.Join(zkTabletPath, "action"),
		path.Join(zkTabletPath, "actionlog"),
	}

	for _, zkPath := range zkPaths {
		_, _, err := zkts.zconn.Get(zkPath)
		if err != nil {
			return err
		}
	}
	return nil
}

func (zkts *ZkTopologyServer) GetTablet(alias naming.TabletAlias) (string, int, error) {
	zkTabletPath := tabletPathForAlias(alias)
	data, stat, err := zkts.zconn.Get(zkTabletPath)
	if err != nil {
		return "", 0, err
	}
	return data, stat.Version(), nil
}

func (zkts *ZkTopologyServer) GetTabletsByCell(cell string) ([]naming.TabletAlias, error) {
	zkTabletsPath := tabletDirectoryForCell(cell)
	children, _, err := zkts.zconn.Children(zkTabletsPath)
	if err != nil {
		return nil, err
	}

	sort.Strings(children)
	result := make([]naming.TabletAlias, len(children))
	for i, child := range children {
		result[i].Cell = cell
		result[i].Uid, err = naming.ParseUid(child)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

//
// Serving Graph management
//
func zkPathForVtKeyspace(cell, keyspace string) string {
	return fmt.Sprintf("/zk/%v/vt/ns/%v", cell, keyspace)
}

func zkPathForVtShard(cell, keyspace, shard string) string {
	return path.Join(zkPathForVtKeyspace(cell, keyspace), shard)
}

func zkPathForVtName(cell, keyspace, shard string, tabletType naming.TabletType) string {
	return path.Join(zkPathForVtShard(cell, keyspace, shard), string(tabletType))
}

func (zkts *ZkTopologyServer) GetSrvTabletTypesPerShard(cell, keyspace, shard string) ([]naming.TabletType, error) {
	zkSgShardPath := zkPathForVtShard(cell, keyspace, shard)
	children, _, err := zkts.zconn.Children(zkSgShardPath)
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNONODE) {
			err = naming.ErrNoNode
		}
		return nil, err
	}
	result := make([]naming.TabletType, len(children))
	for i, tt := range children {
		result[i] = naming.TabletType(tt)
	}
	return result, nil
}

func (zkts *ZkTopologyServer) UpdateSrvTabletType(cell, keyspace, shard string, tabletType naming.TabletType, addrs *naming.VtnsAddrs) error {
	path := zkPathForVtName(cell, keyspace, shard, tabletType)
	data := jscfg.ToJson(addrs)
	_, err := zk.CreateRecursive(zkts.zconn, path, data, 0, zookeeper.WorldACL(zookeeper.PERM_ALL))
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNODEEXISTS) {
			// Node already exists - just stomp away. Multiple writers shouldn't be here.
			// We use RetryChange here because it won't update the node unnecessarily.
			f := func(oldValue string, oldStat zk.Stat) (string, error) {
				return data, nil
			}
			err = zkts.zconn.RetryChange(path, 0, zookeeper.WorldACL(zookeeper.PERM_ALL), f)
		}
	}
	return err
}

func (zkts *ZkTopologyServer) GetSrvTabletType(cell, keyspace, shard string, tabletType naming.TabletType) (*naming.VtnsAddrs, error) {
	path := zkPathForVtName(cell, keyspace, shard, tabletType)
	data, stat, err := zkts.zconn.Get(path)
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNONODE) {
			err = naming.ErrNoNode
		}
		return nil, err
	}
	return naming.NewVtnsAddrs(data, stat.Version())
}

func (zkts *ZkTopologyServer) DeleteSrvTabletType(cell, keyspace, shard string, tabletType naming.TabletType) error {
	path := zkPathForVtName(cell, keyspace, shard, tabletType)
	return zkts.zconn.Delete(path, -1)
}

func (zkts *ZkTopologyServer) UpdateSrvShard(cell, keyspace, shard string, srvShard *naming.SrvShard) error {
	path := zkPathForVtShard(cell, keyspace, shard)
	data := jscfg.ToJson(srvShard)
	_, err := zkts.zconn.Set(path, data, -1)
	return err
}

func (zkts *ZkTopologyServer) GetSrvShard(cell, keyspace, shard string) (*naming.SrvShard, error) {
	path := zkPathForVtShard(cell, keyspace, shard)
	data, stat, err := zkts.zconn.Get(path)
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNONODE) {
			err = naming.ErrNoNode
		}
		return nil, err
	}
	return naming.NewSrvShard(data, stat.Version())
}

func (zkts *ZkTopologyServer) UpdateSrvKeyspace(cell, keyspace string, srvKeyspace *naming.SrvKeyspace) error {
	path := zkPathForVtKeyspace(cell, keyspace)
	data := jscfg.ToJson(srvKeyspace)
	_, err := zkts.zconn.Set(path, data, -1)
	return err
}

func (zkts *ZkTopologyServer) GetSrvKeyspace(cell, keyspace string) (*naming.SrvKeyspace, error) {
	path := zkPathForVtKeyspace(cell, keyspace)
	data, stat, err := zkts.zconn.Get(path)
	if err != nil {
		if zookeeper.IsError(err, zookeeper.ZNONODE) {
			err = naming.ErrNoNode
		}
		return nil, err
	}
	return naming.NewSrvKeyspace(data, stat.Version())
}

var skipUpdateErr = fmt.Errorf("skip update")

func (zkts *ZkTopologyServer) updateTabletEndpoint(oldValue string, oldStat zk.Stat, addr *naming.VtnsAddr) (newValue string, err error) {
	if oldStat == nil {
		// The incoming object doesn't exist - we haven't been placed in the serving
		// graph yet, so don't update. Assume the next process that rebuilds the graph
		// will get the updated tablet location.
		return "", skipUpdateErr
	}

	var addrs *naming.VtnsAddrs
	if oldValue != "" {
		addrs, err = naming.NewVtnsAddrs(oldValue, oldStat.Version())
		if err != nil {
			return
		}

		foundTablet := false
		for i, entry := range addrs.Entries {
			if entry.Uid == addr.Uid {
				foundTablet = true
				if !naming.VtnsAddrEquality(&entry, addr) {
					addrs.Entries[i] = *addr
				}
				break
			}
		}

		if !foundTablet {
			addrs.Entries = append(addrs.Entries, *addr)
		}
	} else {
		addrs = naming.NewAddrs()
		addrs.Entries = append(addrs.Entries, *addr)
	}
	return jscfg.ToJson(addrs), nil
}

func (zkts *ZkTopologyServer) UpdateTabletEndpoint(cell, keyspace, shard string, tabletType naming.TabletType, addr *naming.VtnsAddr) error {
	path := zkPathForVtName(cell, keyspace, shard, tabletType)
	f := func(oldValue string, oldStat zk.Stat) (string, error) {
		return zkts.updateTabletEndpoint(oldValue, oldStat, addr)
	}
	err := zkts.zconn.RetryChange(path, 0, zookeeper.WorldACL(zookeeper.PERM_ALL), f)
	if err == skipUpdateErr {
		err = nil
	}
	return err
}

//
// Remote Tablet Actions
//

func (zkts *ZkTopologyServer) WriteTabletAction(tabletAlias naming.TabletAlias, contents string) (string, error) {
	// Action paths end in a trailing slash to that when we create
	// sequential nodes, they are created as children, not siblings.
	actionPath := tabletActionPathForAlias(tabletAlias) + "/"
	return zkts.zconn.Create(actionPath, contents, zookeeper.SEQUENCE, zookeeper.WorldACL(zookeeper.PERM_ALL))
}

func (zkts *ZkTopologyServer) WaitForTabletAction(actionPath string, waitTime time.Duration, interrupted chan struct{}) (string, error) {
	timer := time.NewTimer(waitTime)
	defer timer.Stop()

	// see if the file exists or sets a watch
	// the loop is to resist zk disconnects while we're waiting
	actionLogPath := strings.Replace(actionPath, "/action/", "/actionlog/", 1)
wait:
	for {
		var retryDelay <-chan time.Time
		stat, watch, err := zkts.zconn.ExistsW(actionLogPath)
		if err != nil {
			delay := 5*time.Second + time.Duration(rand.Int63n(55e9))
			relog.Warning("unexpected zk error, delay retry %v: %v", delay, err)
			// No one likes a thundering herd.
			retryDelay = time.After(delay)
		} else if stat != nil {
			// file exists, go on
			break wait
		}

		// if the file doesn't exist yet, wait for creation event.
		// On any other event we'll retry the ExistsW
		select {
		case actionEvent := <-watch:
			if actionEvent.Type == zookeeper.EVENT_CREATED {
				break wait
			} else {
				// Log unexpected events. Reconnects are
				// handled by zk.Conn, so calling ExistsW again
				// will handle a disconnect.
				relog.Warning("unexpected zk event: %v", actionEvent)
			}
		case <-retryDelay:
			continue wait
		case <-timer.C:
			return "", naming.ErrTimeout
		case <-interrupted:
			return "", naming.ErrInterrupted
		}
	}

	// the node exists, read it
	data, _, err := zkts.zconn.Get(actionLogPath)
	if err != nil {
		return "", fmt.Errorf("action err: %v %v", actionLogPath, err)
	}

	return data, nil
}

func (zkts *ZkTopologyServer) PurgeTabletActions(tabletAlias naming.TabletAlias, canBePurged func(data string) bool) error {
	actionPath := tabletActionPathForAlias(tabletAlias)
	return zkts.PurgeActions(actionPath, canBePurged)
}
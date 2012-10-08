package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/dustin/gomemcached"
	"github.com/dustin/gomemcached/client"
)

type BlobOwnership struct {
	OID    string               `json:"oid"`
	Length int64                `json:"length"`
	Nodes  map[string]time.Time `json:"nodes"`
	Type   string               `json:"type"`
}

type internodeCommand uint8

const (
	removeObjectCmd = internodeCommand(iota)
	acquireObjectCmd
	fetchObjectCmd
)

type internodeTask struct {
	node     StorageNode
	cmd      internodeCommand
	oid      string
	prevNode string
}

var taskWorkers = flag.Int("taskWorkers", 4,
	"Number of blob move/removal workers.")

func (b BlobOwnership) ResolveNodes() NodeList {
	keys := make([]string, 0, len(b.Nodes))
	for k := range b.Nodes {
		keys = append(keys, "/"+k)
	}
	resps := couchbase.GetBulk(keys)

	rv := make(NodeList, 0, len(resps))

	for k, v := range resps {
		if v.Status == gomemcached.SUCCESS {
			a := StorageNode{}
			err := json.Unmarshal(v.Body, &a)
			if err == nil {
				a.name = k[1:]
				rv = append(rv, a)
			}
		}
	}

	sort.Sort(rv)

	return rv
}

// Get the most recent storer of a blob
func (b BlobOwnership) mostRecent() (string, time.Time) {
	rvnode := ""
	rvt := time.Time{}

	for node, t := range b.Nodes {
		if t.After(rvt) {
			rvnode = node
			rvt = t
		}
	}

	return rvnode, rvt
}

func (b BlobOwnership) ResolveRemoteNodes() NodeList {
	return b.ResolveNodes().minusLocal()
}

func getBlobOwnership(oid string) (BlobOwnership, error) {
	rv := BlobOwnership{}
	oidkey := "/" + oid
	err := couchbase.Get(oidkey, &rv)
	return rv, err
}

func copyBlob(w io.Writer, oid string) error {
	f, err := openBlob(oid)
	if err == nil {
		// Doing it locally
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	} else {
		// Doing it remotely
		c := captureResponseWriter{w: w}
		return getBlobFromRemote(&c, oid, http.Header{}, *cachePercentage)
	}
	panic("unreachable")
}

func recordBlobOwnership(h string, l int64, force bool) error {
	k := "/" + h
	err := couchbase.Do(k, func(mc *memcached.Client, vb uint16) error {
		_, err := mc.CAS(vb, k, func(in []byte) ([]byte, memcached.CasOp) {
			ownership := BlobOwnership{}
			err := json.Unmarshal(in, &ownership)
			if err == nil {
				if _, ok := ownership.Nodes[serverId]; ok && !force {
					// Skip it fast if it already knows us
					return nil, memcached.CASQuit
				}
				ownership.Nodes[serverId] = time.Now().UTC()
			} else {
				ownership.Nodes = map[string]time.Time{
					serverId: time.Now().UTC(),
				}
			}
			ownership.OID = h
			ownership.Length = l
			ownership.Type = "blob"
			return mustEncode(&ownership), memcached.CASStore
		}, 0)
		return err
	})
	if err == memcached.CASQuit {
		err = nil
	}
	return err
}

func recordBlobAccess(h string) {
	_, err := couchbase.Incr("/"+h+"/r", 1, 1, 0)
	if err != nil {
		log.Printf("Error incrementing counter for %v: %v", h, err)
	}

	_, err = couchbase.Incr("/"+serverId+"/r", 1, 1, 0)
	if err != nil {
		log.Printf("Error incrementing node identifier: %v", err)
	}
}

// Returns the number of known owners (-1 if it can't be determined)
func removeBlobOwnershipRecord(h, node string) int {
	log.Printf("Cleaning up %v from %v", h, node)
	numOwners := -1

	k := "/" + h
	err := couchbase.Do(k, func(mc *memcached.Client, vb uint16) error {
		_, err := mc.CAS(vb, k, func(in []byte) ([]byte, memcached.CasOp) {
			ownership := BlobOwnership{}

			if len(in) == 0 {
				return nil, memcached.CASQuit
			}

			err := json.Unmarshal(in, &ownership)
			if err == nil {
				delete(ownership.Nodes, node)
			} else {
				log.Printf("Error unmarhaling blob removal from %s: %v",
					in, err)
				return nil, memcached.CASQuit
			}

			var rv []byte
			op := memcached.CASStore

			numOwners = len(ownership.Nodes)

			if len(ownership.Nodes) == 0 && node == serverId {
				op = memcached.CASDelete
			} else {
				rv = mustEncode(&ownership)
			}

			return rv, op
		}, 0)
		return err
	})
	if err != nil && err != memcached.CASQuit {
		log.Printf("Error cleaning %v from %v: %v", node, h, err)
		numOwners = -1
	}
	if numOwners == 0 {
		couchbase.Delete(k + "/r")
	}

	return numOwners
}

func maybeRemoveBlobOwnership(h string) (rv error) {
	log.Printf("Conditionally removing %v", h)

	k := "/" + h
	removedLast := false
	err := couchbase.Do(k, func(mc *memcached.Client, vb uint16) error {
		_, err := mc.CAS(vb, k, func(in []byte) ([]byte, memcached.CasOp) {
			ownership := BlobOwnership{}
			removedLast = false

			if len(in) == 0 {
				return nil, memcached.CASQuit
			}

			err := json.Unmarshal(in, &ownership)
			if err == nil {
				if time.Since(ownership.Nodes[serverId]) < time.Hour {
					rv = errors.New("too soon")
					return nil, memcached.CASQuit
				}
				if len(ownership.Nodes)-1 < globalConfig.MinReplicas {
					rv = errors.New("Insufficient replicas")
					return nil, memcached.CASQuit
				}
				delete(ownership.Nodes, serverId)
			} else {
				log.Printf("Error unmarhaling blob removal from %s: %v",
					in, err)
				rv = err
				return nil, memcached.CASQuit
			}

			var newv []byte
			op := memcached.CASStore

			if len(ownership.Nodes) == 0 {
				removedLast = true
				op = memcached.CASDelete
			} else {
				newv = mustEncode(&ownership)
			}

			return newv, op
		}, 0)
		return err
	})
	if err != nil && err != memcached.CASQuit {
		log.Printf("Error cleaning %v: %v", h, err)
	}
	if removedLast {
		couchbase.Delete(k + "/r")
	}

	return
}

func increaseReplicaCount(oid string, length int64, by int) error {
	nl, err := findAllNodes()
	if err != nil {
		return err
	}
	onto := nl.candidatesFor(oid, NodeList{})
	if len(onto) > by {
		onto = onto[:by]
	}
	for _, n := range onto {
		log.Printf("Asking %v to acquire %v", n, oid)
		queueBlobAcquire(n, oid, "")
	}
	return nil
}

func ensureMinimumReplicaCount() error {
	return runMarkedTask("ensureMinReplCount",
		[]string{"garbageCollectBlobs", "trimFullNodes"},
		ensureMinimumReplicaCountTask)
}

func ensureMinimumReplicaCountTask() error {
	// Don't let this run concurrently with the garbage collector.
	// They don't get along.
	for taskRunning("garbageCollectBlobs") {
		log.Printf("Waiting for gc to finish for ensureMinReplCount")
		time.Sleep(5 * time.Second)
	}

	nl, err := findAllNodes()
	if err != nil {
		return err
	}

	viewRes := struct {
		Rows []struct {
			Id string
		}
	}{}

	// Don't bother trying to replicate to more nodes than exist.
	endKey := globalConfig.MinReplicas - 1
	if globalConfig.MinReplicas > len(nl) {
		endKey = len(nl) - 1
	}

	// Find some less replicated docs to suck in.
	err = couchbase.ViewCustom("cbfs", "repcounts",
		map[string]interface{}{
			"reduce":   false,
			"limit":    globalConfig.ReplicationCheckLimit,
			"startkey": 1,
			"endkey":   endKey,
			"stale":    false,
		},
		&viewRes)

	if err != nil {
		return err
	}

	log.Printf("Increasing replica count of %v items",
		len(viewRes.Rows))

	for _, r := range viewRes.Rows {
		salvageBlob(r.Id[1:], "", nl)
	}
	return nil
}

func pruneBlob(oid string, nodemap map[string]string, nl NodeList) {
	if len(nodemap) <= globalConfig.MaxReplicas {
		log.Printf("Asked to prune a blob that has too few replicas: %v",
			oid)
	}

	log.Printf("Pruning blob %v down from %v repls to %v",
		oid, len(nodemap), globalConfig.MaxReplicas)

	nm := map[string]StorageNode{}
	for _, n := range nl {
		nm[n.name] = n
	}

	remaining := len(nodemap)
	for n := range nodemap {
		if remaining <= globalConfig.MaxReplicas {
			break
		}
		remaining--
		if sn, ok := nm[n]; ok {
			queueBlobRemoval(sn, oid)
		}
	}

}

func pruneExcessiveReplicas() error {
	nl, err := findAllNodes()
	if err != nil {
		return err
	}

	viewRes := struct {
		Rows []struct {
			Id  string
			Doc struct {
				Json struct {
					Nodes map[string]string
				}
			}
		}
	}{}

	// Find some less replicated docs to suck in.
	err = couchbase.ViewCustom("cbfs", "repcounts",
		map[string]interface{}{
			"descending":   true,
			"reduce":       false,
			"include_docs": true,
			"limit":        globalConfig.ReplicationCheckLimit,
			"endkey":       globalConfig.MaxReplicas + 1,
			"stale":        false,
		},
		&viewRes)

	if err != nil {
		return err
	}

	log.Printf("Decreasing replica count of %v items",
		len(viewRes.Rows))

	// Short-circuit when there's nothing to clean
	if len(viewRes.Rows) == 0 {
		return nil
	}

	for _, r := range viewRes.Rows {
		pruneBlob(r.Id[1:], r.Doc.Json.Nodes, nl)
	}
	return nil
}

func hasBlob(oid string) bool {
	_, err := os.Stat(hashFilename(*root, oid))
	return err == nil
}

func performFetch(oid, prev string) {
	c := captureResponseWriter{w: ioutil.Discard}

	// If we already have it, we don't need it more.
	st, err := os.Stat(hashFilename(*root, oid))
	if err == nil {
		err = recordBlobOwnership(oid, st.Size(), false)
		if err != nil {
			log.Printf("Error recording fetched blob: %v",
				err)
		}
		return
	}

	err = getBlobFromRemote(&c, oid, http.Header{}, 100)

	if err == nil && c.statusCode == 200 {
		if prev != "" {
			log.Printf("Removing ownership of %v from %v after takeover",
				oid, prev)
			n, err := findNode(prev)
			if err != nil {
				log.Printf("Error finding old node: %v", err)
				removeBlobOwnershipRecord(oid, prev)
			} else {
				log.Printf("Forcing post-move blob removal of %v from %v",
					oid, n)
				queueBlobRemoval(n, oid)
			}
		}
	} else {
		log.Printf("Error grabbing remote object, got %v/%v",
			c.statusCode, err)
	}
}

func salvageBlob(oid, deadNode string, nl NodeList) {
	candidates := nl.candidatesFor(oid,
		NodeList{nl.named(deadNode)})

	if len(candidates) == 0 {
		log.Printf("Couldn't find a candidate for blob!")
	} else {
		log.Printf("Recommending %v get a copy of %v",
			candidates[0], oid)
		queueBlobAcquire(candidates[0], oid, deadNode)
	}
}

var internodeTaskQueue = make(chan internodeTask, 1000)

func internodeTaskWorker() {
	for c := range internodeTaskQueue {
		switch c.cmd {
		case removeObjectCmd:
			if err := c.node.deleteBlob(c.oid); err != nil {
				log.Printf("Error deleting %v from %v: %v",
					c.oid, c.node, err)
				if c.node.IsDead() {
					log.Printf("Node is dead, cleaning")
					removeBlobOwnershipRecord(c.oid,
						c.node.name)
				}
			}
		case acquireObjectCmd:
			if err := c.node.acquireBlob(c.oid, c.prevNode); err != nil {
				log.Printf("Error acquiring %v from %v: %v",
					c.oid, c.node, err)
			}
		case fetchObjectCmd:
			performFetch(c.oid, c.prevNode)
		default:
			log.Fatalf("Unhandled worker task: %v", c)
		}
	}
}

func initTaskQueueWorkers() {
	for i := 0; i < *taskWorkers; i++ {
		go internodeTaskWorker()
	}
}

func queueBlobRemoval(n StorageNode, oid string) {
	internodeTaskQueue <- internodeTask{
		node: n,
		cmd:  removeObjectCmd,
		oid:  oid,
	}
}

// Ask a remote node to go get a blob
func queueBlobAcquire(n StorageNode, oid string, prev string) {
	internodeTaskQueue <- internodeTask{
		node:     n,
		cmd:      acquireObjectCmd,
		oid:      oid,
		prevNode: prev,
	}
}

// Ask this node to go get a blob
func queueBlobFetch(oid, prev string) {
	internodeTaskQueue <- internodeTask{
		cmd:      fetchObjectCmd,
		oid:      oid,
		prevNode: prev,
	}
}

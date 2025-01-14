// Copyright 2015 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"go.etcd.io/etcd/pkg/v3/idutil"
	"go.etcd.io/etcd/pkg/v3/wait"
	"go.etcd.io/etcd/raft/v3/raftpb"
)

func getSnapshotFn() (func() ([]byte, error), <-chan struct{}) {
	snapshotTriggeredC := make(chan struct{})
	return func() ([]byte, error) {
		snapshotTriggeredC <- struct{}{}
		return nil, nil
	}, snapshotTriggeredC
}

type cluster struct {
	peers              []string
	commitC            []<-chan *commit
	errorC             []<-chan error
	proposeC           []chan *Propose
	confChangeC        []chan raftpb.ConfChange
	snapshotTriggeredC []<-chan struct{}
}

// newCluster creates a cluster of n nodes
func newCluster(n int) *cluster {
	peers := make([]string, n)
	for i := range peers {
		peers[i] = fmt.Sprintf("http://127.0.0.1:%d", 10000+i)
	}

	clus := &cluster{
		peers:              peers,
		commitC:            make([]<-chan *commit, len(peers)),
		errorC:             make([]<-chan error, len(peers)),
		proposeC:           make([]chan *Propose, len(peers)),
		confChangeC:        make([]chan raftpb.ConfChange, len(peers)),
		snapshotTriggeredC: make([]<-chan struct{}, len(peers)),
	}

	for i := range clus.peers {
		os.RemoveAll(fmt.Sprintf("raftexample-%d", i+1))
		os.RemoveAll(fmt.Sprintf("raftexample-%d-snap", i+1))
		clus.proposeC[i] = make(chan *Propose, 1)
		clus.confChangeC[i] = make(chan raftpb.ConfChange, 1)
		fn, snapshotTriggeredC := getSnapshotFn()
		clus.snapshotTriggeredC[i] = snapshotTriggeredC
		clus.commitC[i], clus.errorC[i], _ = newRaftNode(i+1, clus.peers, false, fn, clus.proposeC[i], clus.confChangeC[i])
	}

	return clus
}

// Close closes all cluster nodes and returns an error if any failed.
func (clus *cluster) Close() (err error) {
	for i := range clus.peers {
		go func(i int) {
			for range clus.commitC[i] {
				// drain pending commits
			}
		}(i)
		close(clus.proposeC[i])
		// wait for channel to close
		if erri := <-clus.errorC[i]; erri != nil {
			err = erri
		}
		// clean intermediates
		os.RemoveAll(fmt.Sprintf("raftexample-%d", i+1))
		os.RemoveAll(fmt.Sprintf("raftexample-%d-snap", i+1))
	}
	return err
}

func (clus *cluster) closeNoErrors(t *testing.T) {
	t.Log("closing cluster...")
	if err := clus.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log("closing cluster [done]")
}

// TestProposeOnCommit starts three nodes and feeds commits back into the proposal
// channel. The intent is to ensure blocking on a proposal won't block raft progress.
func TestProposeOnCommit(t *testing.T) {
	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	donec := make(chan struct{})
	for i := range clus.peers {
		// feedback for "n" committed entries, then update donec
		go func(idx int, pC chan<- *Propose, cC <-chan *commit, eC <-chan error) {
			for n := 0; n < 100; n++ {
				_, ok := <-cC
				if !ok {
					pC = nil
				}
				select {
				case pC <- &Propose{[]byte(fmt.Sprintf("%d-foo-%d", idx, n))}:
					continue
				case err := <-eC:
					t.Errorf("%d-eC message (%v)", idx, err)
				}
			}
			donec <- struct{}{}
			for range cC {
				// acknowledge the commits from other nodes so
				// raft continues to make progress
			}
		}(i, clus.proposeC[i], clus.commitC[i], clus.errorC[i])

		// one message feedback per node
	}
	go func(i int) { clus.proposeC[i] <- &Propose{[]byte("foo")} }(0)

	for range clus.peers {
		<-donec
	}
}

// TestCloseProposerBeforeReplay tests closing the producer before raft starts.
func TestCloseProposerBeforeReplay(t *testing.T) {
	clus := newCluster(1)
	// close before replay so raft never starts
	defer clus.closeNoErrors(t)
}

// TestCloseProposerInflight tests closing the producer while
// committed messages are being published to the client.
func TestCloseProposerInflight(t *testing.T) {
	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	// some inflight ops
	go func() {
		clus.proposeC[0] <- &Propose{[]byte("foo")}
		clus.proposeC[0] <- &Propose{[]byte("bar")}
	}()

	// wait for one message
	if c, ok := <-clus.commitC[1]; !ok || !bytes.Equal(c.data[0], []byte("foo")) {
		t.Fatalf("Commit failed")
	}
}

func TestPutAndGetKeyValue(t *testing.T) {
	os.RemoveAll("raftexample-1")
	os.RemoveAll("raftexample-1-snap")
	defer func() {
		os.RemoveAll("raftexample-1")
		os.RemoveAll("raftexample-1-snap")
	}()
	clusters := []string{"http://127.0.0.1:9021"}

	proposeC := make(chan *Propose)
	defer close(proposeC)

	confChangeC := make(chan raftpb.ConfChange)
	defer close(confChangeC)

	var kvs *kvstore
	getSnapshot := func() ([]byte, error) { return kvs.getSnapshot() }
	commitC, errorC, snapshotterReady := newRaftNode(1, clusters, false, getSnapshot, proposeC, confChangeC)

	kvs = newKVStore(1, <-snapshotterReady, proposeC, commitC, errorC, wait.New(), idutil.NewGenerator(1, time.Now()))

	srv := httptest.NewServer(&httpKVAPI{
		store:       kvs,
		confChangeC: confChangeC,
	})
	defer srv.Close()

	// wait server started
	<-time.After(time.Second * 3)

	wantKey, wantValue := "test-key", "test-value"
	url := fmt.Sprintf("%s/%s", srv.URL, wantKey)
	body := bytes.NewBufferString(wantValue)
	cli := srv.Client()

	req, err := http.NewRequest("PUT", url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	_, err = cli.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := cli.Get(url)
	if err != nil {
		t.Fatal(err)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotValue := string(data); wantValue != gotValue {
		t.Fatalf("expect %s, got %s", wantValue, gotValue)
	}
}

// TestAddNewNode tests adding new node to the existing cluster.
func TestAddNewNode(t *testing.T) {
	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	os.RemoveAll("raftexample-4")
	os.RemoveAll("raftexample-4-snap")
	defer func() {
		os.RemoveAll("raftexample-4")
		os.RemoveAll("raftexample-4-snap")
	}()

	newNodeURL := "http://127.0.0.1:10004"
	clus.confChangeC[0] <- raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  4,
		Context: []byte(newNodeURL),
	}

	proposeC := make(chan *Propose)
	defer close(proposeC)

	confChangeC := make(chan raftpb.ConfChange)
	defer close(confChangeC)

	newRaftNode(4, append(clus.peers, newNodeURL), true, nil, proposeC, confChangeC)

	go func() {
		proposeC <- &Propose{[]byte("foo")}
	}()

	if c, ok := <-clus.commitC[0]; !ok || !bytes.Equal(c.data[0], []byte("foo")) {
		t.Fatalf("Commit failed")
	}
}

func TestSnapshot(t *testing.T) {
	prevDefaultSnapshotCount := defaultSnapshotCount
	prevSnapshotCatchUpEntriesN := snapshotCatchUpEntriesN
	defaultSnapshotCount = 4
	snapshotCatchUpEntriesN = 4
	defer func() {
		defaultSnapshotCount = prevDefaultSnapshotCount
		snapshotCatchUpEntriesN = prevSnapshotCatchUpEntriesN
	}()

	clus := newCluster(3)
	defer clus.closeNoErrors(t)

	go func() {
		clus.proposeC[0] <- &Propose{[]byte("foo")}
	}()

	c := <-clus.commitC[0]

	select {
	case <-clus.snapshotTriggeredC[0]:
		t.Fatalf("snapshot triggered before applying done")
	default:
	}
	close(c.applyDoneC)
	<-clus.snapshotTriggeredC[0]
}

func TestPutAndGetKeyValueOnPeer(t *testing.T) {
	var clusters []string
	for i := 1; i <= 3; i++ {
		clusters = append(clusters, fmt.Sprintf("http://127.0.0.1:902%d", i))
		dataDir := fmt.Sprintf("raftexample-%d", i)
		snapDir := fmt.Sprintf("raftexample-%d-snap", i)
		os.RemoveAll(dataDir)
		os.RemoveAll(snapDir)
		defer func() {
			os.RemoveAll(dataDir)
			os.RemoveAll(snapDir)
		}()
		proposeC := make(chan *Propose)
		defer close(proposeC)
		confChangeC := make(chan raftpb.ConfChange)
		defer close(confChangeC)
		var kvs *kvstore
		getSnapshot := func() ([]byte, error) { return kvs.getSnapshot() }
		commitC, errorC, snapshotterReady := newRaftNode(i, clusters, false, getSnapshot, proposeC, confChangeC)
		kvs = newKVStore(uint16(i), <-snapshotterReady, proposeC, commitC, errorC, wait.New(), idutil.NewGenerator(uint16(i), time.Now()))
		go serveHttpKVAPI(kvs, 19020+i, confChangeC, errorC)
	}

	// wait server started
	<-time.After(time.Second * 3)

	wantKey, wantValue := "test-key", "test-value"
	url1 := fmt.Sprintf("%s/%s", "http://localhost:19021", wantKey)
	url2 := fmt.Sprintf("%s/%s", "http://localhost:19022", wantKey)
	body := bytes.NewBufferString(wantValue)

	req, err := http.NewRequest("PUT", url1, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/html; charset=utf-8")
	_, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.DefaultClient.Get(url2)
	if err != nil {
		t.Fatal(err)
	}

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if gotValue := string(data); wantValue != gotValue {
		t.Errorf("expect %s, got %s", wantValue, gotValue)
	}
}

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"encoding/json"

	"github.com/golang/glog"
	"github.com/samuel/go-zookeeper/zk"
)

const (
	zkPath = "/aurora/scheduler"
	zkLeaderPrefix = "member_"
)

type finder interface {
	leaderURL() (string, error)
}

func newFinder(url string) (f finder, err error) {
	if strings.HasPrefix(url, "http://") {
		f = &httpFinder{url: url}
	}

	if strings.HasPrefix(url, "zk://") {
		f = newZkFinder(url)
	}

	if f == nil {
		err = errors.New("finder: bad address")
	}

	return f, err
}

type httpFinder struct {
	url string
}

func (f *httpFinder) leaderURL() (string, error) {
	// This will redirect us to the elected Aurora master
	schedulerURL := fmt.Sprintf("%s/scheduler", f.url)
	rr, err := http.NewRequest("GET", schedulerURL, nil)
	if err != nil {
		return "", err
	}

	rresp, err := httpClient.Transport.RoundTrip(rr)
	if err != nil {
		return "", err
	}
	defer rresp.Body.Close()

	masterLoc := rresp.Header.Get("Location")
	if masterLoc == "" {
		glog.V(6).Info("missing Location header in request")
		masterLoc = schedulerURL
	}

	return strings.TrimRight(masterLoc, "/scheduler"), nil
}

func hostsFromURL(urls string) (hosts []string, err error) {
	for _, s := range strings.Split(urls, ",") {
		u, err := url.Parse(s)
		if err != nil {
			return hosts, err
		}

		hosts = append(hosts, u.Host)
	}

	return hosts, err
}

type zkFinder struct {
	conn     *zk.Conn

	sync.RWMutex
	leaderIP string
	leaderPort int
}

func newZkFinder(url string) *zkFinder {
	zkSrvs, err := hostsFromURL(url)
	if err != nil {
		panic(err)
	}

	conn, events, err := zk.Connect(zkSrvs, 20 * time.Second)
	if err != nil {
		panic(err)
	}

	go func() {
		for ev := range events {
			glog.V(6).Infof("conn: %s server: %s", ev.State, ev.Server)
		}
	}()

	f := zkFinder{conn: conn}
	go f.watch()

	return &f
}

func (f *zkFinder) leaderzNode() (string, error) {
	children, stat, err := f.conn.Children(zkPath)
	if stat == nil {
		err = errors.New("zkFinder: children returned nil stat")
	}
	if err != nil {
		return "", err
	}

	var leaderSeq int
	var leader string
	for _, child := range children {
		path := strings.Split(child, zkLeaderPrefix)
		if len(path) > 1 {
			seq, err := strconv.Atoi(path[1])
			if err != nil {
				return "", err
			}

			if leader == "" {
				leader = child
			}

			if seq <= leaderSeq {
				leaderSeq = seq
				leader = child
			}
		}
	}

	if leader == "" {
		return leader, errors.New("zkFinder: zNode not found")
	}

	return fmt.Sprintf("%s/%s", zkPath, leader), nil
}

func (f *zkFinder) leaderURL() (string, error) {
	f.RLock()
	defer f.RUnlock()

	if f.leaderIP == "" {
		return "", errors.New("zkFinder: no leader found via ZooKeeper")
	}

	return fmt.Sprintf("http://%s:%d", f.leaderIP, f.leaderPort), nil
}
type entity struct {
	ServiceEndpoint     endpoint            `json:"serviceEndpoint"`
	AdditionalEndpoints map[string]endpoint `json:"additionalEndpoints"` // unused
	Status              string              `json:"status"`
}

type endpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}


// possible endpoint statuses. Currently only concerned with ALIVE.
const (
	statusDead = "DEAD"
	statusStarting = "STARTING"
	statusAlive = "ALIVE"
	statusStopping = "STOPPING"
	statusStopped = "STOPPED"
	statusWarning = "WARNING"
	statusUnknown = "UNKNOWN"
)

func (f *zkFinder) watch() {
	for _ = range time.NewTicker(1 * time.Second).C {
		zNode, err := f.leaderzNode()
		if err != nil {
			glog.Warning(err)
			continue
		}

		glog.V(6).Info("leader zNode at: ", zNode)

		data, stat, events, err := f.conn.GetW(zNode)
		if stat == nil {
			err = errors.New("get returned nil stat")
		}
		if err != nil {
			glog.Warning(err)
			continue
		}
		dec := json.NewDecoder(strings.NewReader(string(data)))
		f.Lock()
		var e entity
		// decode an array value (Message)
		err = dec.Decode(&e)
		if err != nil {
			glog.Fatal(err)
		}
		f.leaderIP = string(e.ServiceEndpoint.Host)
		f.leaderPort = e.ServiceEndpoint.Port
		f.Unlock()

		for ev := range events {
			switch {
			case ev.Err != nil:
				err = fmt.Errorf("watcher error %+v", ev.Err)
			case ev.Type == zk.EventNodeDeleted:
				err = errors.New("leader zNode deleted")
			}

			if err != nil {
				glog.Warning(err)
				break
			}
		}
	}
}

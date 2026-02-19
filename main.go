package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// route maps an FQDN to a container backend.
type route struct {
	Host string
	Port string
}

var (
	mu          sync.RWMutex
	routes      = map[string]route{}
	networkName string // discovered at startup
)

// dockerClient talks to the Docker daemon over the unix socket.
var dockerClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", "/var/run/docker.sock")
		},
	},
}

func main() {
	var err error
	networkName, err = detectNetwork()
	if err != nil {
		log.Fatalf("detect network: %v", err)
	}
	log.Printf("using network %q", networkName)

	go watchEvents()
	log.Println("listening on :80")
	log.Fatal(http.ListenAndServe(":80", http.HandlerFunc(proxy)))
}

func proxy(w http.ResponseWriter, r *http.Request) {
	host := strings.Split(r.Host, ":")[0]

	mu.RLock()
	rt, ok := routes[host]
	mu.RUnlock()

	if !ok {
		http.Error(w, fmt.Sprintf("no backend for %s", host), http.StatusBadGateway)
		return
	}

	target, _ := url.Parse(fmt.Sprintf("http://%s:%s", rt.Host, rt.Port))
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
}

func watchEvents() {
	// Initial scan on startup.
	if err := scan(); err != nil {
		log.Printf("scan: %v", err)
	}

	for {
		if err := streamEvents(); err != nil {
			log.Printf("events: %v", err)
		}
		time.Sleep(time.Second) // back off before reconnecting
	}
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

// detectNetwork reads our own container ID from /etc/hostname, inspects
// ourselves via the Docker API, and returns the first non-default network.
func detectNetwork() (string, error) {
	hostname, err := os.ReadFile("/etc/hostname")
	if err != nil {
		return "", fmt.Errorf("read /etc/hostname: %w", err)
	}
	cid := strings.TrimSpace(string(hostname))

	var info dockerInspect
	if err := dockerGet("/containers/"+cid+"/json", &info); err != nil {
		return "", fmt.Errorf("inspect self: %w", err)
	}

	for name := range info.NetworkSettings.Networks {
		if name != "bridge" && name != "host" && name != "none" {
			return name, nil
		}
	}
	return "", fmt.Errorf("no custom network found on container %s", cid)
}

// tracked holds container IDs currently on our network.
var tracked = map[string]struct{}{}

func streamEvents() error {
	// Listen for network connect/disconnect and container start/stop/die.
	// filters: {"type":["network","container"],"event":["connect","disconnect","start","stop","die"]}
	resp, err := dockerClient.Get("http://localhost/events?filters=%7B%22type%22%3A%5B%22network%22%2C%22container%22%5D%2C%22event%22%3A%5B%22connect%22%2C%22disconnect%22%2C%22start%22%2C%22stop%22%2C%22die%22%5D%7D")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	for {
		var ev dockerEvent
		if err := dec.Decode(&ev); err != nil {
			return err
		}

		switch {
		// Network connect/disconnect on the current network.
		case ev.Type == "network" && ev.Actor.Attributes["name"] == networkName:
			cid := ev.Actor.Attributes["container"]
			if ev.Action == "connect" {
				tracked[cid] = struct{}{}
				log.Printf("event: network connect %s", cid[:12])
			} else {
				delete(tracked, cid)
				log.Printf("event: network disconnect %s", cid[:12])
			}
			if err := scan(); err != nil {
				log.Printf("scan: %v", err)
			}

		// Container lifecycle — only scan if this container is tracked.
		case ev.Type == "container":
			if _, ok := tracked[ev.Actor.ID]; ok {
				log.Printf("event: container %s %s", ev.Action, ev.Actor.ID[:12])
				if err := scan(); err != nil {
					log.Printf("scan: %v", err)
				}
			}
		}
	}
}

// Docker API response types (just the fields we need).
type dockerContainer struct {
	ID string `json:"Id"`
}

type dockerInspect struct {
	Config struct {
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func dockerGet(path string, out interface{}) error {
	resp, err := dockerClient.Get("http://localhost" + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

func scan() error {
	var containers []dockerContainer
	filter := fmt.Sprintf("/containers/json?filters={%%22network%%22:%%5B%%22%s%%22%%5D}", networkName)
	if err := dockerGet(filter, &containers); err != nil {
		log.Printf("containers: %v", err)
		return err
	}

	// Seed tracked set so we recognise future lifecycle events.
	for _, c := range containers {
		tracked[c.ID] = struct{}{}
	}

	next := map[string]route{}

	for _, c := range containers {
		var info dockerInspect
		if err := dockerGet("/containers/"+c.ID+"/json", &info); err != nil {
			log.Printf("inspect %s: %v", c.ID[:12], err)
			continue
		}

		fqdn := ""
		explicitPort := ""
		for _, env := range info.Config.Env {
			if strings.HasPrefix(env, "SUB2PORT=") {
				val := strings.TrimPrefix(env, "SUB2PORT=")
				if host, p, err := net.SplitHostPort(val); err == nil {
					fqdn = host
					explicitPort = p
				} else {
					fqdn = val
				}
				break
			}
		}
		if fqdn == "" {
			continue
		}

		nw, ok := info.NetworkSettings.Networks[networkName]
		if !ok || nw.IPAddress == "" {
			continue
		}

		// Use explicit port from SUB2PORT, else first exposed port, else 80.
		port := "80"
		if explicitPort != "" {
			port = explicitPort
		} else {
			for p := range info.Config.ExposedPorts {
				port = strings.Split(p, "/")[0] // "8080/tcp" -> "8080"
				break
			}
		}

		next[fqdn] = route{Host: nw.IPAddress, Port: port}
		log.Printf("update %s: %v", fqdn, next[fqdn])
	}

	mu.Lock()
	routes = next
	mu.Unlock()
	return nil
}

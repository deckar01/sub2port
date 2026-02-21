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

type ContainerID string
type ContainerName string
type HostName string

// Docker API

type dockerContainer struct {
	ID ContainerID `json:"Id"`
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         ContainerID       `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
}

type dockerInspect struct {
	Name   string `json:"Name"`
	Config struct {
		Env          []string            `json:"Env"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// Types

type route struct {
	Name ContainerName
	Host string
	Port string
}

type hostEntry struct {
	backends []route
	counter  uint64
}

type binding struct {
	Domain HostName
	Name   ContainerName
}

type routeTable struct {
	sync.RWMutex
	hosts      map[HostName]*hostEntry
	containers map[ContainerID][]binding
}

// State

var networkName string
var hostPort string

var table = routeTable{
	hosts:      make(map[HostName]*hostEntry),
	containers: make(map[ContainerID][]binding),
}

var networkQuery string
var eventsQuery = "http://localhost" + dockerQuery("/events", map[string][]string{
	"type":  {"network", "container"},
	"event": {"connect", "disconnect", "start"},
})

// dockerClient talks to the Docker daemon over the unix socket.
var dockerClient = &http.Client{
	Transport: &http.Transport{
		DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			return net.Dial("unix", "/var/run/docker.sock")
		},
	},
}

// Router

func main() {
	var err error
	networkName, hostPort, err = detectNetwork()
	if err != nil {
		log.Fatalf("detect network: %v", err)
	}
	log.Printf("# using network %q", networkName)
	networkQuery = dockerQuery("/containers/json", map[string][]string{
		"network": {networkName},
	})

	go watchEvents()
	log.Printf("# listening on :%s", hostPort)
	log.Fatal(http.ListenAndServe(":80", http.HandlerFunc(proxy)))
}

// Inspect the network name and host port
func detectNetwork() (string, string, error) {
	hostname, err := os.ReadFile("/etc/hostname")
	if err != nil {
		return "", "", fmt.Errorf("read /etc/hostname: %w", err)
	}
	containerID := strings.TrimSpace(string(hostname))

	var container dockerInspect
	if err := dockerGet("/containers/"+containerID+"/json", &container); err != nil {
		return "", "", fmt.Errorf("inspect self: %w", err)
	}

	var network string
	for name := range container.NetworkSettings.Networks {
		if name != "bridge" && name != "host" && name != "none" {
			network = name
			break
		}
	}
	if network == "" {
		return "", "", fmt.Errorf("no custom network found on container %s", containerID)
	}

	// Detect the host port mapped to the container.
	port := "80"
	for _, bindings := range container.NetworkSettings.Ports {
		for _, binding := range bindings {
			if binding.HostPort != "" {
				port = binding.HostPort
				break
			}
		}
		if port != "80" {
			break
		}
	}

	return network, port, nil
}

func proxy(writer http.ResponseWriter, request *http.Request) {
	host := HostName(strings.Split(request.Host, ":")[0])

	table.Lock()
	entry := table.hosts[host]
	if entry == nil {
		table.Unlock()
		http.Error(writer, fmt.Sprintf("no backend for %s", host), http.StatusBadGateway)
		return
	}
	idx := entry.counter % uint64(len(entry.backends))
	entry.counter++
	backend := entry.backends[idx]
	table.Unlock()

	target, _ := url.Parse(fmt.Sprintf("http://%s:%s", backend.Host, backend.Port))
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(writer, request)
}

func watchEvents() {
	// Initial scan on startup.
	var containers []dockerContainer
	if err := dockerGet(networkQuery, &containers); err != nil {
		log.Printf("containers: %v", err)
	}

	for _, container := range containers {
		table.Lock()
		table.containers[container.ID] = nil
		table.Unlock()
		addRoutes(container.ID)
	}

	for {
		if err := eventLoop(); err != nil {
			log.Printf("events: %v", err)
		}
		time.Sleep(time.Second) // back off before reconnecting
	}
}

// Listen for docker events
func eventLoop() error {
	response, err := dockerClient.Get(eventsQuery)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	jsonDecoder := json.NewDecoder(response.Body)
	for {
		var event dockerEvent
		if err := jsonDecoder.Decode(&event); err != nil {
			return err
		}

		switch {
		// Track containers that are connected to the current network
		case event.Type == "network" && event.Actor.Attributes["name"] == networkName:
			containerID := ContainerID(event.Actor.Attributes["container"])
			if event.Action == "connect" {
				table.Lock()
				table.containers[containerID] = nil
				table.Unlock()
			} else {
				removeRoutes(containerID)
			}

		// Wait for the container to start before routing
		case event.Type == "container":
			// Ignore out of network containers
			table.RLock()
			_, ok := table.containers[event.Actor.ID]
			table.RUnlock()
			if !ok {
				continue
			}
			addRoutes(event.Actor.ID)
		}
	}
}

func dockerGet(path string, out interface{}) error {
	response, err := dockerClient.Get("http://localhost" + path)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	return json.NewDecoder(response.Body).Decode(out)
}

// Escape JSON queries for the Docker API
func dockerQuery(path string, filters interface{}) string {
	query, _ := json.Marshal(filters)
	return path + "?filters=" + url.QueryEscape(string(query))
}

// Parse a container's route config
func addRoutes(containerID ContainerID) {
	var container dockerInspect
	if err := dockerGet("/containers/"+string(containerID)+"/json", &container); err != nil {
		log.Printf("inspect %s: %v", containerID[:12], err)
		return
	}

	var config string
	for _, env := range container.Config.Env {
		if strings.HasPrefix(env, "SUB2PORT=") {
			config = strings.TrimPrefix(env, "SUB2PORT=")
			break
		}
	}
	if config == "" {
		return
	}

	network, ok := container.NetworkSettings.Networks[networkName]
	if !ok || network.IPAddress == "" {
		return
	}

	name := ContainerName(strings.TrimPrefix(container.Name, "/"))

	defaultPort := "80"
	for _port := range container.Config.ExposedPorts {
		defaultPort = strings.Split(_port, "/")[0] // "8080/tcp" -> "8080"
		break
	}

	var bindings []binding
	table.Lock()
	for _, entry := range strings.Split(config, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		domain, port := entry, defaultPort
		if _domain, _port, err := net.SplitHostPort(entry); err == nil {
			domain = _domain
			port = _port
		}
		hostName := HostName(domain)
		entry := table.hosts[hostName]
		if entry == nil {
			entry = &hostEntry{}
			table.hosts[hostName] = entry
		}
		entry.backends = append(entry.backends, route{Name: name, Host: network.IPAddress, Port: port})
		bindings = append(bindings, binding{Domain: hostName, Name: name})
		log.Printf("+ %s (%d) -> %s:%s", domain, len(entry.backends), name, port)
	}
	table.containers[containerID] = bindings
	table.Unlock()
}

func removeRoutes(containerID ContainerID) {
	table.Lock()
	for _, binding := range table.containers[containerID] {
		entry := table.hosts[binding.Domain]
		if entry == nil {
			continue
		}
		for i, route := range entry.backends {
			if route.Name == binding.Name {
				log.Printf("- %s (%d) -> %s:%s", binding.Domain, len(entry.backends)-1, route.Name, route.Port)
				entry.backends = append(entry.backends[:i], entry.backends[i+1:]...)
				break
			}
		}
		if len(entry.backends) == 0 {
			delete(table.hosts, binding.Domain)
		}
	}
	delete(table.containers, containerID)
	table.Unlock()
}

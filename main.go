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

// route maps a domain to a container backend.
type route struct {
	Name string
	Host string
	Port string
}

type dockerContainer struct {
	ID string `json:"Id"`
}

type dockerEvent struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
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

var routesLock sync.RWMutex
var routes = map[string]route{}
var networkName string
var hostPort string
var nodes = map[string]struct{}{}
var registry = map[string][]string{}

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
		for _, b := range bindings {
			if b.HostPort != "" {
				port = b.HostPort
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
	host := strings.Split(request.Host, ":")[0]

	routesLock.RLock()
	route, ok := routes[host]
	routesLock.RUnlock()

	if !ok {
		http.Error(writer, fmt.Sprintf("no backend for %s", host), http.StatusBadGateway)
		return
	}

	target, _ := url.Parse(fmt.Sprintf("http://%s:%s", route.Host, route.Port))
	httputil.NewSingleHostReverseProxy(target).ServeHTTP(writer, request)
}

func watchEvents() {
	// Initial scan on startup.
	var containers []dockerContainer
	if err := dockerGet(networkQuery, &containers); err != nil {
		log.Printf("containers: %v", err)
	}

	for _, container := range containers {
		nodes[container.ID] = struct{}{}
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
	defer response.Body.Close()

	jsonDecoder := json.NewDecoder(response.Body)
	for {
		var event dockerEvent
		if err := jsonDecoder.Decode(&event); err != nil {
			return err
		}

		switch {
		// Track containers that are connected to the current network
		case event.Type == "network" && event.Actor.Attributes["name"] == networkName:
			containerID := event.Actor.Attributes["container"]
			if event.Action == "connect" {
				nodes[containerID] = struct{}{}
			} else {
				// Remove routes when containers disconnect
				delete(nodes, containerID)
				removeRoutes(containerID)
			}

		// Wait for the container to start before routing
		case event.Type == "container":
			// Ignore out of network containers
			if _, ok := nodes[event.Actor.ID]; !ok {
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
	defer response.Body.Close()
	return json.NewDecoder(response.Body).Decode(out)
}

// Escape JSON queries for the Docker API
func dockerQuery(path string, filters interface{}) string {
	query, _ := json.Marshal(filters)
	return path + "?filters=" + url.QueryEscape(string(query))
}

// Parse a container's route config
func addRoutes(containerID string) {
	var container dockerInspect
	if err := dockerGet("/containers/"+containerID+"/json", &container); err != nil {
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

	name := strings.TrimPrefix(container.Name, "/")

	// Default port: first exposed port, else 80.
	defaultPort := "80"
	for _port := range container.Config.ExposedPorts {
		defaultPort = strings.Split(_port, "/")[0] // "8080/tcp" -> "8080"
		break
	}

	var domains []string
	routesLock.Lock()
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
		routes[domain] = route{Name: name, Host: network.IPAddress, Port: port}
		domains = append(domains, domain)
		log.Printf("+ %s -> %s:%s", domain, name, port)
	}
	registry[containerID] = domains
	routesLock.Unlock()
}

// Remove routes to a container
func removeRoutes(containerID string) {
	routesLock.Lock()
	for _, domain := range registry[containerID] {
		if route, ok := routes[domain]; ok {
			log.Printf("- %s -> %s:%s", domain, route.Name, route.Port)
			delete(routes, domain)
		}
	}
	delete(registry, containerID)
	routesLock.Unlock()
}

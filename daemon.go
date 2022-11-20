package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"inet.af/tcpproxy"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"tailscale.com/tsnet"
	"time"
)

type responseWriter struct {
	http.ResponseWriter
	StatusCode int
}

func (w *responseWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	w.StatusCode = code
}

func withLogging(h http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := &responseWriter{
			ResponseWriter: w,
			StatusCode:     http.StatusOK,
		}
		h.ServeHTTP(ww, r)
		log.Printf("[%s] [%v] [%d] %s %s %s", r.Method, time.Since(start), ww.StatusCode, r.Host, r.URL.Path, r.URL.RawQuery)
	}
}

func withOnlyMethod(method string, f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "405 method not allowed", http.StatusMethodNotAllowed)
			return
		}
		f(w, r)
	}
}

type Instance struct {
	tsServer     *tsnet.Server
	containerID  string
	Name         string   `json:"name"`
	Image        string   `json:"image"`
	Port         int      `json:"port"`
	TailscaleIPs []string `json:"tailscale_ips"`
}

type Daemon struct {
	instanceLock sync.Mutex
	instances    map[string]*Instance
	docker       *client.Client
	tsAuthKey    string
	server       *http.Server
}

func newDaemon() (*Daemon, error) {
	docker, err := client.NewClientWithOpts(client.FromEnv)
	if err != nil {
		return nil, err
	}
	tsAuthKey := os.Getenv("TS_AUTHKEY")
	if tsAuthKey == "" {
		return nil, fmt.Errorf("TS_AUTHKEY is required")
	}
	d := &Daemon{
		instanceLock: sync.Mutex{},
		instances:    make(map[string]*Instance),
		docker:       docker,
		tsAuthKey:    tsAuthKey,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/run", withOnlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		port, err := strconv.Atoi(r.URL.Query().Get("port"))
		if err != nil {
			log.Printf("invalid port: %v", err)
			http.Error(w, "invalid port", http.StatusBadRequest)
		}
		instanceSpec := Instance{
			Name:  r.URL.Query().Get("name"),
			Image: r.URL.Query().Get("image"),
			Port:  port,
		}
		if instanceSpec.Name == "" || instanceSpec.Image == "" || instanceSpec.Port == 0 {
			log.Printf("invalid instance spec: %v", instanceSpec)
			http.Error(w, "invalid instance spec", http.StatusBadRequest)
			return
		}
		d.instanceLock.Lock()
		if _, ok := d.instances[instanceSpec.Name]; ok {
			log.Printf("instance %s already exists", instanceSpec.Name)
			http.Error(w, "instance already exists", http.StatusBadRequest)
			d.instanceLock.Unlock()
			return
		}
		d.instanceLock.Unlock()
		createdInstance, err := d.launchInstance(r.Context(), instanceSpec.Name, instanceSpec.Image, instanceSpec.Port)
		if err != nil {
			log.Printf("failed to launch instance: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err = json.NewEncoder(w).Encode(createdInstance); err != nil {
			log.Printf("failed to write response: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))

	mux.HandleFunc("/list", withOnlyMethod(http.MethodGet, func(w http.ResponseWriter, r *http.Request) {
		d.instanceLock.Lock()
		defer d.instanceLock.Unlock()
		instances := make([]Instance, 0, len(d.instances))
		for _, instance := range d.instances {
			lc, err := instance.tsServer.LocalClient()
			if err != nil {
				log.Printf("failed to get local client: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			status, err := lc.Status(r.Context())
			if err != nil {
				log.Printf("failed to get status: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			instance.TailscaleIPs = make([]string, len(status.TailscaleIPs))
			for i, ip := range status.TailscaleIPs {
				instance.TailscaleIPs[i] = ip.String()
			}
			instances = append(instances, *instance)
		}
		if err := json.NewEncoder(w).Encode(instances); err != nil {
			log.Printf("failed to write response: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))

	mux.HandleFunc("/stop", withOnlyMethod(http.MethodDelete, func(w http.ResponseWriter, r *http.Request) {
		d.instanceLock.Lock()
		defer d.instanceLock.Unlock()
		name := r.URL.Query().Get("name")
		if _, ok := d.instances[name]; !ok {
			log.Printf("instance %s not found", name)
			http.Error(w, "instance not found", http.StatusNotFound)
			return
		}
		if err := d.stopInstance(r.Context(), name); err != nil {
			log.Printf("failed to stop instance: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}))

	server := &http.Server{
		Addr:    daemonAddr,
		Handler: withLogging(mux),
	}
	d.server = server
	return d, nil
}

func (d *Daemon) stop() {
	ctx := context.Background()
	d.instanceLock.Lock()
	defer d.instanceLock.Unlock()
	for _, i := range d.instances {
		if err := d.stopInstance(ctx, i.Name); err != nil {
			log.Printf("failed to stop intsance %s: %v", i.Name, err)
		}
	}
	if err := d.server.Shutdown(ctx); err != nil {
		log.Printf("failed to stop http server: %v", err)
	}
}

func (d *Daemon) run() {
	log.Printf("starting daemon on %s", d.server.Addr)
	if err := d.server.ListenAndServe(); err != http.ErrServerClosed {
		log.Printf("http server listen: %v", err)
	}
}

func (d *Daemon) stopInstance(ctx context.Context, name string) error {
	instance, ok := d.instances[name]
	if !ok {
		return fmt.Errorf("instance %s not found", name)
	}
	go func() {
		if err := instance.tsServer.Close(); err != nil {
			log.Printf("failed to stop ts server: %v", err)
		}
	}()
	timeout := 5 * time.Second
	if err := d.docker.ContainerStop(ctx, instance.containerID, &timeout); err != nil {
		return err
	}
	if err := d.docker.ContainerRemove(ctx, instance.containerID, types.ContainerRemoveOptions{}); err != nil {
		return err
	}
	delete(d.instances, name)
	log.Printf("deleted instance %s", name)
	return nil
}

func (d *Daemon) launchInstance(ctx context.Context, name, image string, port int) (*Instance, error) {
	freePort, err := func() (port int, err error) {
		var a *net.TCPAddr
		if a, err = net.ResolveTCPAddr("tcp", "0.0.0.0:0"); err == nil {
			var l *net.TCPListener
			if l, err = net.ListenTCP("tcp", a); err == nil {
				defer l.Close()
				return l.Addr().(*net.TCPAddr).Port, nil
			}
		}
		return
	}()
	if err != nil {
		return nil, err
	}
	resp, err := d.docker.ContainerCreate(ctx, &container.Config{
		Image: image,
		ExposedPorts: nat.PortSet{
			nat.Port(strconv.Itoa(port)): struct{}{},
		},
	}, &container.HostConfig{
		PortBindings: map[nat.Port][]nat.PortBinding{
			nat.Port(strconv.Itoa(port)): {{
				HostIP:   "0.0.0.0",
				HostPort: strconv.Itoa(freePort)},
			},
		},
	}, nil, &specs.Platform{}, name)
	if err != nil {
		return nil, err
	}

	confDir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(confDir, "ts", name)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &tsnet.Server{
		AuthKey:   d.tsAuthKey,
		Hostname:  name,
		Ephemeral: true,
		Dir:       dir,
	}
	tsListener, err := s.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, err
	}
	lc, err := s.LocalClient()
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := tsListener.Accept()
			if err != nil {
				log.Printf("accept error: %v", err)
				return
			}
			user, err := lc.WhoIs(context.Background(), c.RemoteAddr().String())
			if err != nil {
				log.Printf("failed to get user: %v", err)
				return
			}
			log.Printf("user %v connected", user.UserProfile)
			go tcpproxy.To(fmt.Sprintf("0.0.0.0:%d", freePort)).HandleConn(c)
		}

	}()
	if err = d.docker.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
		return nil, err
	}
	d.instanceLock.Lock()
	defer d.instanceLock.Unlock()
	i := &Instance{
		tsServer:    s,
		containerID: resp.ID,
		Name:        name,
		Image:       image,
		Port:        port,
	}
	d.instances[i.Name] = i
	return i, nil
}

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"

	//	"runtime/debug"
	"regexp"
	"strings"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v3"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"golang.org/x/net/proxy"
)

const DESTINATION_KEY = "Destinations"
const PEERS_KEY = "Peers"
const MY_DOMAIN_KEY = "Domain"

var ro_regex = []string{
	`.*\.eu$`,
}

type Configuration struct {
	Proxy        []Proxies `yaml:"proxy"`
	Host         string    `yaml:"host"`
	Port         string    `yaml:"port"`
	SSLPort      string    `yaml:"sslPort"`
	AllowedHosts []string  `yaml:"allowedHosts"`
	LocalDomains []string  `yaml:"localDomains"`
	Peers        []string  `yaml:"peers"`
	Password     string    `yaml:"password"`
	OutgoingIp   string    `yaml:"outgoingIp"`
	IPv6Only     bool      `yaml:"ipv6only"`
	ch           chan int
}

type ProxyInfo struct {
	dialer  proxy.Dialer
	Enabled bool
	Host    string
}
type Proxies struct {
	Domains []string `json:"domains"`
	Hosts   []string `json:"hosts"`
	dynamic bool
	info    []ProxyInfo
}

var c Configuration
var mu sync.Mutex

var bufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 5120*1024)
	},
}
var CONFIG_FILE string = "proxy.yaml"

func SaveConfig(c Configuration) {
	b, err := yaml.Marshal(c)
	if err != nil {
		log.Printf("Error converting to JSON %v", err)
	} else {
		_ = ioutil.WriteFile(CONFIG_FILE, b, 0644)
	}

}

func getDefaultConfig() Configuration {
	host, _ := os.Hostname()
	defConfig := Configuration{
		Host:         host,
		Port:         ":8888",
		SSLPort:      ":443",
		AllowedHosts: []string{"10.0.0.0/8", "192.168.0.0/16", "172.16.0.0/12"},
		LocalDomains: []string{`.*\.eu$`},
		Peers:        []string{"https://example_peer_for_eu.com:8889"},
		Password:     "example",
	}
	SaveConfig(defConfig)
	return defConfig
}

func NewConfig() Configuration {
	c := Configuration{}
	b, err := ioutil.ReadFile(CONFIG_FILE)
	if err != nil {
		log.Printf("Failed reading file %v, using default config", err)
		return getDefaultConfig()
	}
	err = yaml.Unmarshal(b, &c)
	if err != nil {
		log.Printf("Failed loading configuration: %v", err)
		return getDefaultConfig()
	}
	c.ch = make(chan int, 100)
	return c
}

func (c *Configuration) GetDestinations() string {
	routes := c.LocalDomains
	for _, p := range c.Proxy {
		if !p.dynamic {
			for _, reg := range p.Domains {
				routes = append(routes, reg)
			}
		}
	}
	return encode(c.LocalDomains)
}

func (c *Configuration) getPeer(peer string) *Proxies {
	hostport, err := url.Parse(peer)
	if err != nil {
		log.Printf("Could not parse peer %v", peer)
		return nil
	}
	for i, p := range c.Proxy {
		for _, h := range p.Hosts {
			ph, _ := url.Parse(h)
			if ph.Host == hostport.Host {
				return &c.Proxy[i]
			}
		}
	}
	return nil
}

func (c *Configuration) AddPeer(peer string, dest []string) {
	u, err := url.Parse(peer)
	if err != nil {
		log.Printf("Could not add peer %v: %v", peer, err)
		return
	}
	if u.User.Username() == "" {
		u.User = url.UserPassword(c.GenUser(), c.GenPassword())
	}
	_, port, _ := net.SplitHostPort(c.Port)
	_, ports, _ := net.SplitHostPort(c.SSLPort)
	if u.Hostname() == c.Host && (u.Port() == port || u.Port() == ports) {
		//		log.Printf("Not adding local process to the list of peers %v", u.String())
		return
	}
	p := c.getPeer(peer)

	if p == nil {
		log.Printf("Adding new peer %v: %v", u.String(), dest)
		p = &Proxies{
			Hosts:   []string{u.String()},
			Domains: dest,
			dynamic: true,
		}
		c.Proxy = append(c.Proxy, *p)
		c.ch <- 1
	} else {
		if len(dest) > 0 {
			if !reflect.DeepEqual(p.Domains, dest) {
				log.Printf("Updating domains for peer %v to %v", getProxyName(p.Hosts[0]), dest)
			}

			p.Domains = []string{}
			for _, r := range dest {
				p.Domains = append(p.Domains, r)
			}
		}
	}

	//	log.Printf("Current list of peers %v: %+v %v", len(c.Proxy), c.Proxy[0], p)

}

func (c *Configuration) ProcessPeers(peersString string) {
	peers := []string{}

	ok := decode(peersString, &peers)
	if ok != nil {
		log.Printf("Unable to decode peers: %v: %v", peersString, peers)
		return
	}
	for _, p := range peers {
		c.AddPeer(p, []string{})
	}
	return
}

func (c *Configuration) ProcessDestinations(destinations string, peer string) string {
	routes := []string{}
	dest := []string{}
	ok := decode(destinations, &dest)
	if ok != nil {
		log.Printf("Not processing peers: %v", destinations)
		return ""
	}

	if len(dest) > 0 {
		c.AddPeer(peer, dest)
	}
	return encode(routes)
}

func (c *Configuration) AddHeader(header http.Header) {

	header.Set(DESTINATION_KEY, c.GetDestinations())
	header.Set(PEERS_KEY, c.GetPeers())
	header.Set(MY_DOMAIN_KEY, c.getLocalProxyAddress())

}

func (c *Configuration) LoopPeers() {

	for {
		select {
		case _ = <-c.ch:
		case <-time.After(60 * time.Second):
		}
		for i, p := range c.Proxy {
			for j, host := range p.Hosts {
				if len(c.Proxy[i].info) == j {
					cc := ProxyInfo{Host: host}
					c.Proxy[i].info = append(c.Proxy[i].info, cc)
				}
				conn, err := DialProxy(&c.Proxy[i].info[j], "google.com:443", "127.0.0.1:80")
				log.Printf("testing %v, enabled %v, err %v", getProxyName(c.Proxy[i].info[j].Host), c.Proxy[i].info[j].Enabled, err)
				if err == nil {
					conn.Close()
					if !c.Proxy[i].info[j].Enabled {
						log.Printf("Enabling proxy %+v", c.Proxy[i].info[j].Host)
					}
					c.Proxy[i].info[j].Enabled = true
				} else {
					if c.Proxy[i].info[j].Enabled {
						log.Printf("Disabling proxy %v, err %v", c.Proxy[i].info[j].Host, err)
					}
					c.Proxy[i].info[j].Enabled = false
				}

			}
		}
	}
}

func (c *Configuration) ProcessHeader(header http.Header) {
	//	log.Printf("Got headers %+v", header)
	d := header.Get(MY_DOMAIN_KEY)
	if d == "" {
		return
	}
	c.ProcessDestinations(header.Get(DESTINATION_KEY), d)
	c.ProcessPeers(header.Get(PEERS_KEY))
}

func (c *Configuration) getLocalProxyAddress() string {
	_, port, err := net.SplitHostPort(c.SSLPort)
	if err != nil {
		log.Printf("Unable to parse the listening port %v, %v", c.Port, err)
	}
	return "https://" + c.Host + ":" + port
}

func getProxyName(p string) string {
	if p == "direct" {
		return p
	}
	u, _ := url.Parse(p)
	u.User = nil
	return u.String()
}

func encode(v interface{}) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func decode(v string, i interface{}) error {
	data, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		log.Printf("Decode failed for %v", v)
		return err
	}
	err = json.Unmarshal(data, i)
	if err != nil {
		log.Printf("Unmarshalling json failed for %v: %v", string(data), err)
		return err
	}
	return nil
}

func (c *Configuration) GetPeers() string {
	hosts := []string{}
	for _, p := range c.Proxy {
		if p.dynamic {
			hosts = append(hosts, p.Hosts[0])
		}
	}
	return encode(hosts)
}

func (c *Configuration) GenUser() string {
	return hex.EncodeToString([]byte(c.Password))
}

func (c *Configuration) GenPassword() string {
	return c.Password
}

func validateRequest(r *http.Request) error {
	username, password, ok := r.BasicAuth()
	if ok {
		if ok && username == c.GenUser() && password == c.GenPassword() {
			//			log.Printf("Got authenticated request %v %v %v", username, password, ok)
			return nil
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return fmt.Errorf("Invalid Remove Address: %v", r.RemoteAddr)
	}
	remoteIp := net.ParseIP(host)
	if remoteIp == nil {
		return fmt.Errorf("Could not parse remove address ip: %v", r.RemoteAddr)
	}
	for _, cidr := range c.AllowedHosts {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Printf("Invalid Allowed Host %v, err %v", cidr, err)
		}
		if n.Contains(remoteIp) {
			return nil
		}
	}
	return fmt.Errorf("Access denied for host %v", r.RemoteAddr)
}

func lookupIPv4(host string) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		ipv4 := ip.To4()
		if ipv4 == nil {
			continue
		}
		return ipv4, nil
	}
	return nil, fmt.Errorf("no IPv4 address found for host: %s", host)
}

type Socks struct {
	state int
	proxy string
}

func (s *Socks) Dial(network string, targetAddr string) (_ net.Conn, err error) {
	// dial TCP
	proxy := s.proxy
	conn, err := net.DialTimeout("tcp", proxy, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			conn.Close()
		}
	}()

	// connection request
	host, ports, err := net.SplitHostPort(targetAddr)

	port, _ := strconv.Atoi(ports)
	if err != nil {
		return nil, err
	}
	ip := net.IPv4(0, 0, 0, 1).To4()
	if s.state == 2 {
		ip, err = lookupIPv4(host)
		if err != nil {
			return nil, err
		}
	}
	req := []byte{
		4,                          // version number
		1,                          // command CONNECT
		byte(port >> 8),            // higher byte of destination port
		byte(port),                 // lower byte of destination port (big endian)
		ip[0], ip[1], ip[2], ip[3], // special invalid IP address to indicate the host name is provided
		0, // user id is empty, anonymous proxy only
	}
	if s.state == 0 || s.state == 1 {
		req = append(req, []byte(host+"\x00")...)
	}
	n, err := conn.Write(req)
	if err != nil {
		return nil, fmt.Errorf("unable to write to server %v: %v", proxy, err)
	}
	resp := make([]byte, 8)
	n, err = conn.Read(resp)
	if err != nil {
		return nil, err
	} else if n != 8 {
		return nil, errors.New("server does not respond properly")
	}
	resp = resp[0:n]
	switch resp[1] {
	case 90:
		// if the server supports resolution, use it
		if s.state == 0 {
			s.state = 1
		}
	case 91:
		// server does not support resolution
		if s.state == 0 {
			log.Printf("Switching off name resolution for proxy %v", s.proxy)
			s.state = 2
		}
		return nil, errors.New("socks connection request rejected or failed")
	case 92:
		return nil, errors.New("socks connection request rejected because SOCKS server cannot connect to identd on the client")
	case 93:
		return nil, errors.New("socks connection request rejected because the client program and identd report different user-ids")
	default:
		return nil, errors.New("socks connection request failed, unknown error")
	}

	return conn, nil
}

var cache = map[string][]string{}

func resolveHostIp(from string) []string {
	ip, _, _ := net.SplitHostPort(from)
	if res, ok := cache[ip]; ok {
		return res
	}
	names, err := net.LookupAddr(ip)
	if err != nil {
		names = []string{ip}
	}
	cache[ip] = names
	return cache[ip]
}

func DialProxy(p *ProxyInfo, host string, from string) (net.Conn, error) {
	//	debug.PrintStack()
	names := resolveHostIp(from)
	if p.Host == "direct" {
		dialer := &net.Dialer{
			Timeout: 4 * time.Second,
		}
		if c.OutgoingIp != "" {
			dialer.LocalAddr = &net.TCPAddr{
				IP:   net.ParseIP(c.OutgoingIp),
				Port: 0,
			}
		}
		proto := "tcp"
		if c.IPv6Only == true {
			proto = "tcp6"
		}
		co, err := dialer.Dial(proto, host)
		if err == nil {
			log.Printf("Direct from %+v: %v [ %v ]", names, host, co.RemoteAddr())
		} else {
			log.Printf("Fsiler to connect to %v using proto %v: %v", host, proto, err)
		}

		return co, err
	}
	mu.Lock()
	proxyDialer := p.dialer
	if proxyDialer == nil {
		log.Printf("New dialer for proxy %v", getProxyName(p.Host))
		proxyUrl, err := url.Parse(p.Host)
		if err != nil {
			mu.Unlock()
			return nil, err
		}
		err = fmt.Errorf("Proxy scheme not recognised: %v", proxyUrl.Scheme)
		if proxyUrl.Scheme == "http" || proxyUrl.Scheme == "https" {
			var hd *HTTPConnectDialer
			hd, err = NewHTTPConnectDialer(p.Host)
			c.AddHeader(hd.DefaultHeader)
			proxyDialer = hd

		} else if strings.HasPrefix(proxyUrl.Scheme, "socks") {
			proxyDialer = &Socks{
				proxy: proxyUrl.Host,
			}
			err = nil
		}
		if err != nil {
			fmt.Println("Error connecting to proxy:", err)
			mu.Unlock()
			return nil, err
		}
		p.dialer = proxyDialer
	}
	mu.Unlock()
	log.Printf("%+v: %v -> %v", names, getProxyName(p.Host), host)
	hd, ok := proxyDialer.(*HTTPConnectDialer)
	if !ok {
		return proxyDialer.Dial("tcp", host)
	}
	con, header, err := hd.DialContext(context.Background(), "tcp", host)
	c.ProcessHeader(header)
	return con, err
}

// looks up the host in the list of destinations and returns the longest sized destination
// to allow more specific matches to be return (x.com will be returned instead .com)
// by default uses direct connection (no proxy)
func getConnection(hostPort string, remoteAddr string) (net.Conn, error) {
	outbound := &ProxyInfo{Host: "direct"}
	arr := []*ProxyInfo{}
	max_reg := ""
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		log.Printf("Unable to split %v into host and port: %v", hostPort, err)
		return nil, err
	}

	for i, p := range c.Proxy {
		//	log.Printf("Testing host %v against pattern %v", host, p.Domains)
		for _, reg := range p.Domains {
			r, _ := regexp.Compile(reg)
			if r.MatchString(host) && len(reg) > len(max_reg) {
				new := true
				for j, pr := range p.info {
					if pr.Enabled {
						if new {
							new = false
							arr = []*ProxyInfo{}
						}
						arr = append(arr, &c.Proxy[i].info[j])
						max_reg = reg
					}
				}
			}
		}
	}
	if len(arr) > 0 {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		prob := r.Intn(len(arr))
		outbound = arr[prob]
		//		log.Printf("Identified %d eligible proxies, picked %v: %+v", len(arr), prob, outbound.Host)

	}
	//	log.Printf("Matched host %v pattern %v -> proxy %v (%v proxies)", host, max_reg, outbound, len(c.Proxy))
	con, err := DialProxy(outbound, hostPort, remoteAddr)
	if err != nil && !strings.Contains(err.Error(), "503") {
		log.Printf("Disabling proxy %v, err %v", outbound.Host, err)
		outbound.Enabled = false
	}
	return con, err

}

func formatRequest(r *http.Request) (string, error) {
	// Create return string
	var request []string
	// Add the request string
	url := fmt.Sprintf("%v %v %v", r.Method, r.URL.RequestURI(), r.Proto)
	request = append(request, url)
	// Add the host
	request = append(request, fmt.Sprintf("Host: %v", r.URL.Host))
	// Loop through headers
	for name, headers := range r.Header {
		name = strings.ToLower(name)
		for _, h := range headers {
			request = append(request, fmt.Sprintf("%v: %v", name, h))
		}
	}

	// Return the request as a string
	return strings.Join(request, "\r\n") + "\r\n\r\n", nil
}

func handleTunneling(w http.ResponseWriter, r *http.Request) {
	err := validateRequest(r)
	if err != nil {
		log.Printf("Error authenticating the request %s", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return

	}
	c.ProcessHeader(r.Header)
	host := r.URL.Host
	if r.Method != http.MethodConnect && !strings.Contains(host, ":") {
		host = host + ":80"
	}
	dest_conn, err := getConnection(host, r.RemoteAddr)
	if err != nil {
		log.Printf("Error starting remote connection %s", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodConnect {
		requestDump, err := formatRequest(r)
		if err != nil {
			log.Println(err)
		}
		log.Printf("Writing to %+v:\n%v", dest_conn, string(requestDump))
		dest_conn.Write([]byte(requestDump))
		log.Printf("Done")
	} else {
		c.AddHeader(w.Header())
		w.WriteHeader(http.StatusOK)
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok { // HTTP/2
		if err := proxyBody(dest_conn, w, r); err != nil {
			log.Printf("Error proxying http/2 connection %s", err)
		}
		return

	}
	client_conn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}

	go transfer(dest_conn, client_conn)
	go transfer(client_conn, dest_conn)
}

// flushingIoCopy is analogous to buffering io.Copy(), but also attempts to flush on each iteration.
// If dst does not implement http.Flusher(e.g. net.TCPConn), it will do a simple io.CopyBuffer().
// Reasoning: http2ResponseWriter will not flush on its own, so we have to do it manually.
func flushingIoCopy(dst io.Writer, src io.Reader, buf []byte) (written int64, err error) {
	flusher, ok := dst.(http.Flusher)
	if !ok {
		return io.CopyBuffer(dst, src, buf)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			flusher.Flush()
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er != nil {
			if er != io.EOF {
				err = er
			}
			break
		}
	}
	return
}

// Copies data target->clientReader and clientWriter->target, and flushes as needed
// Returns when clientWriter-> target stream is done.
// Caddy should finish writing target -> clientReader.
func dualStream(target io.ReadWriteCloser, clientReader io.ReadCloser, clientWriter io.Writer) error {
	stream := func(w io.Writer, r io.Reader) error {
		// copy bytes from r to w
		orig := bufferPool.Get()
		buf := orig.([]byte)
		buf = buf[0:cap(buf)]
		_, _err := flushingIoCopy(w, r, buf)
		bufferPool.Put(orig)
		if closeWriter, ok := w.(interface {
			CloseWrite() error
		}); ok {
			closeWriter.CloseWrite()
		}
		return _err
	}

	go stream(target, clientReader)
	return stream(clientWriter, target)
}

func proxyBody(targetConn io.ReadWriteCloser, w http.ResponseWriter, r *http.Request) error {
	defer r.Body.Close()
	wFlusher, ok := w.(http.Flusher)
	if !ok {
		err := fmt.Errorf("ResponseWriter doesn't implement Flusher()")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}
	w.WriteHeader(http.StatusOK)
	wFlusher.Flush()
	return dualStream(targetConn, r.Body, w)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

func defaultHandler(w http.ResponseWriter, r *http.Request) {
	handleTunneling(w, r)
}

func main() {
	c = NewConfig()
	if c.OutgoingIp != "" {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{
					Timeout: time.Millisecond * time.Duration(10000),
					LocalAddr: &net.UDPAddr{
						IP:   net.ParseIP(c.OutgoingIp),
						Port: 0,
					},
				}
				return d.DialContext(ctx, network, address)
			},
		}
	}

	TLS_KEY_FILE := c.Host + ".key"
	TLS_CERT_FILE := c.Host + ".crt"

	_, err := ioutil.ReadFile(TLS_KEY_FILE)
	if err != nil {
		genCert(c.Host, TLS_KEY_FILE, TLS_CERT_FILE)
	}
	// adding the default peers
	if len(c.Peers) > 0 {
		c.ProcessPeers(encode(c.Peers))
	}

	go c.LoopPeers()
	h2s := &http2.Server{}

	server := &http.Server{
		Addr:    c.Port,
		Handler: h2c.NewHandler(http.HandlerFunc(defaultHandler), h2s),
	}
	if c.SSLPort == "" {
		log.Fatal(server.ListenAndServe())
	} else {
		go server.ListenAndServe()

		server = &http.Server{
			Addr:      c.SSLPort,
			Handler:   http.HandlerFunc(defaultHandler),
			TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}
		log.Fatal(server.ListenAndServeTLS(TLS_CERT_FILE, TLS_KEY_FILE))
	}
}

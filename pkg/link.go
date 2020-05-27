package pkg

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"

	//goCIDR "github.com/apparentlymart/go-cidr/cidr"
	"github.com/pion/dtls/v2"
	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
	"golang.zx2c4.com/wireguard/tun"
)

const (
	printGreen = "\033[1;32m%s\033[0m"
	bufferSize = 1500
	defaultMTU = 1420
)

type vpnLink struct {
	sync.Mutex
	name              string
	routesReady       bool
	serverRoutesReady bool
	link              netlink.Link
	iface             myTun
	conn              myConn
	resolvConf        []byte
	ret               error
	errChan           chan error
	upChan            chan bool
	nameChan          chan string
	termChan          chan os.Signal
	serverIPs         []net.IP
	localIPv4         net.IP
	serverIPv4        net.IP
	localIPv6         net.IP
	serverIPv6        net.IP
	mtu               []byte
	mtuInt            uint16
	gateways          []net.IP
}

type myConn interface {
	Write([]byte) (int, error)
	Read([]byte) (int, error)
	Close() error
}

type myTun struct {
	tun.Device
	myConn
	readBuf  []byte
	writeBuf []byte
}

func (t *myTun) Read(b []byte) (int, error) {
	if t.Device != nil {
		// unix.IFF_NO_PI is not set, therefore we receive packet information
		n, err := t.Device.File().Read(b)
		if n < 4 {
			return 0, err
		}
		// TODO: better shift
		// probably something like
		// https://github.com/songgao/water/blob/2b4b6d7c09d80835e5f13f6b040d69f00a158b24/syscalls_darwin.go#L224
		for i := 0; i < n-4; i++ {
			b[i] = b[i+4]
		}
		return n - 4, nil
	}
	return t.myConn.Read(b)
}

func (t *myTun) Write(b []byte) (int, error) {
	if t.Device != nil {
		return t.Device.Write(append(make([]byte, 4), b...), 4)
	}
	return t.myConn.Write(b)
}

// init a TLS connection
func initConnection(server string, config *Config, favorite *Favorite) (*vpnLink, error) {
	// TLS
	getUrl := fmt.Sprintf("https://%s/myvpn?sess=%s&hostname=%s&hdlc_framing=%s&ipv4=%s&ipv6=%s&Z=%s",
		server,
		favorite.Object.SessionID,
		base64.StdEncoding.EncodeToString([]byte("my-hostname")),
		Bool(config.PPPD),
		favorite.Object.IPv4,
		Bool(config.IPv6 && bool(favorite.Object.IPv6)),
		favorite.Object.UrZ,
	)

	serverIPs, err := net.LookupIP(server)
	if err != nil || len(serverIPs) == 0 {
		return nil, fmt.Errorf("failed to resolve %s: %s", server, err)
	}

	// define link channels
	link := &vpnLink{
		serverIPs: serverIPs,
		errChan:   make(chan error, 1),
		upChan:    make(chan bool, 1),
		nameChan:  make(chan string, 1),
		termChan:  make(chan os.Signal, 1),
	}

	if config.DTLS && favorite.Object.TunnelDTLS {
		s := fmt.Sprintf("%s:%s", server, favorite.Object.TunnelPortDTLS)
		log.Printf("Connecting to %s using DTLS", s)
		addr, err := net.ResolveUDPAddr("udp", s)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve UDP address: %s", err)
		}
		conf := &dtls.Config{
			InsecureSkipVerify: config.InsecureTLS,
		}
		link.conn, err = dtls.Dial("udp4", addr, conf)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s:%s: %s", server, favorite.Object.TunnelPortDTLS, err)
		}
	} else {
		conf := &tls.Config{
			InsecureSkipVerify: config.InsecureTLS,
		}
		link.conn, err = tls.Dial("tcp", fmt.Sprintf("%s:443", server), conf)
		if err != nil {
			return nil, fmt.Errorf("failed to dial %s:443: %s", server, err)
		}

		req, err := http.NewRequest("GET", getUrl, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create VPN session request: %s", err)
		}
		req.Header.Set("User-Agent", userAgentVPN)
		err = req.Write(link.conn)
		if err != nil {
			return nil, fmt.Errorf("failed to send VPN session request: %s", err)
		}

		if debug {
			log.Printf("URL: %s", getUrl)
		}

		resp, err := http.ReadResponse(bufio.NewReader(link.conn), nil)
		if err != nil {
			return nil, fmt.Errorf("failed to get initial VPN connection response: %s", err)
		}
		resp.Body.Close()

		link.localIPv4 = net.ParseIP(resp.Header.Get("X-VPN-client-IP"))
		link.serverIPv4 = net.ParseIP(resp.Header.Get("X-VPN-server-IP"))
		link.localIPv6 = net.ParseIP(resp.Header.Get("X-VPN-client-IPv6"))
		link.serverIPv6 = net.ParseIP(resp.Header.Get("X-VPN-server-IPv6"))

		if debug {
			log.Printf("Client IP: %s", link.localIPv4)
			log.Printf("Server IP: %s", link.serverIPv4)
			if link.localIPv6 != nil {
				log.Printf("Client IPv6: %s", link.localIPv6)
			}
			if link.localIPv6 != nil {
				log.Printf("Server IPv6: %s", link.serverIPv6)
			}
		}
	}

	if !config.PPPD {
		if config.Water {
			log.Printf("Using water module to create tunnel")
			device, err := water.New(water.Config{
				DeviceType: water.TUN,
			})
			if err != nil {
				return nil, fmt.Errorf("failed to create a %q interface: %s", water.TUN, err)
			}

			link.name = device.Name()
			log.Printf("Created %s interface", link.name)
			link.iface = myTun{myConn: device}
		} else {
			log.Printf("Using wireguard module to create tunnel")
			device, err := tun.CreateTUN("", defaultMTU)
			if err != nil {
				return nil, fmt.Errorf("failed to create an interface: %s", err)
			}

			link.name, err = device.Name()
			if err != nil {
				return nil, fmt.Errorf("failed to get an interface name: %s", err)
			}
			log.Printf("Created %s interface", link.name)
			link.iface = myTun{Device: device}
		}
	}

	return link, nil
}

// error handler
func (l *vpnLink) errorHandler() {
	l.ret = <-l.errChan
	l.termChan <- syscall.SIGINT
}

func cidrContainsIPs(cidr *net.IPNet, ips []net.IP) bool {
	for _, ip := range ips {
		if cidr.Contains(ip) {
			//net, ok := goCIDR.PreviousSubnet(&net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}, 17)
			//log.Printf("Previous: %s %t", net, ok)
			return true
		}
	}
	return false
}

// wait for pppd and config DNS and routes
func (l *vpnLink) waitAndConfig(config *Config, fav *Favorite) {
	var err error
	// wait for tun name
	l.name = <-l.nameChan
	if l.name == "" {
		l.errChan <- fmt.Errorf("failed to detect tunnel name")
		return
	}

	if config.PPPD {
		// wait for tun up
		if !<-l.upChan {
			l.errChan <- fmt.Errorf("unexpected tun status event")
			return
		}
	}

	l.Lock()
	defer l.Unlock()
	// read current resolv.conf
	// reading it here in order to avoid conflicts, when the second VPN connection is established in parallel
	l.resolvConf, err = ioutil.ReadFile(resolvPath)
	if err != nil {
		l.errChan <- fmt.Errorf("cannot read %s: %s", resolvPath, err)
		return
	}

	// define DNS servers, provided by F5
	log.Printf("Setting %s", resolvPath)
	config.vpnDNSServers = fav.Object.DNS
	dns := bytes.NewBufferString("# created by gof5 VPN client\n")
	if len(config.DNS) == 0 {
		log.Printf("Forwarding DNS requests to %q", config.vpnDNSServers)
		for _, v := range fav.Object.DNS {
			if _, err = dns.WriteString("nameserver " + v.String() + "\n"); err != nil {
				l.errChan <- fmt.Errorf("failed to write DNS entry into buffer: %s", err)
				return
			}
		}
	} else {
		listenAddr := startDns(l, config)
		if _, err = dns.WriteString("nameserver " + listenAddr + "\n"); err != nil {
			l.errChan <- fmt.Errorf("failed to write DNS entry into buffer: %s", err)
			return
		}
	}
	if fav.Object.DNSSuffix != "" {
		if _, err = dns.WriteString("search " + fav.Object.DNSSuffix + "\n"); err != nil {
			l.errChan <- fmt.Errorf("failed to write search DNS entry into buffer: %s", err)
			return
		}
	}
	if err = ioutil.WriteFile(resolvPath, dns.Bytes(), 0644); err != nil {
		l.errChan <- fmt.Errorf("failed to write %s: %s", resolvPath, err)
		return
	}

	// set routes
	log.Printf("Setting routes on %s interface", l.name)
	if !config.PPPD {
		if err := setInterface(l); err != nil {
			l.errChan <- err
			return
		}
	}

	// set F5 gateway route
	for _, dst := range l.serverIPs {
		gws, err := routeGet(dst)
		if err != nil {
			l.errChan <- err
			return
		}
		for _, gw := range gws {
			if err = routeAdd(dst, gw, 1, l.name); err != nil {
				l.errChan <- err
				return
			}
			l.gateways = append(l.gateways, gw)
		}
		l.serverRoutesReady = true
	}

	// set custom routes
	for _, cidr := range config.Routes {
		if false && cidrContainsIPs(cidr, l.serverIPs) {
			log.Printf("Skipping %s subnet", cidr)
			//continue
		}
		if err = routeAdd(cidr, nil, 0, l.name); err != nil {
			l.errChan <- err
			return
		}
	}
	l.routesReady = true
	log.Printf(printGreen, "Connection established")
}

// restore config
func (l *vpnLink) restoreConfig(config *Config) {
	l.Lock()
	defer l.Unlock()

	defer func() {
		if l.iface.Device != nil {
			l.iface.Device.Close()
		}
		if l.iface.myConn != nil {
			l.iface.myConn.Close()
		}
	}()

	if l.resolvConf != nil {
		log.Printf("Restoring original %s", resolvPath)
		if err := ioutil.WriteFile(resolvPath, l.resolvConf, 0644); err != nil {
			log.Printf("Failed to restore %s: %s", resolvPath, err)
		}
	}

	if l.serverRoutesReady {
		// remove F5 gateway route
		for _, dst := range l.serverIPs {
			for _, gw := range l.gateways {
				if err := routeDel(dst, gw, 1, l.name); err != nil {
					log.Print(err)
				}
			}
		}
	}

	if l.routesReady {
		if l.ret == nil {
			log.Printf("Removing routes from %s interface", l.name)
			for _, cidr := range config.Routes {
				if false && cidrContainsIPs(cidr, l.serverIPs) {
					log.Printf("Skipping %s subnet", cidr)
					//continue
				}
				if err := routeDel(cidr, nil, 0, l.name); err != nil {
					log.Print(err)
				}
			}
		}
	}
}

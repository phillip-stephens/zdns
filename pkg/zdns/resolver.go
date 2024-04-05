/*
 * ZDNS Copyright 2024 Regents of the University of Michigan
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy
 * of the License at http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
 * implied. See the License for the specific language governing
 * permissions and limitations under the License.
 */

package zdns

import (
	"fmt"
	log "github.com/sirupsen/logrus"
	"github.com/zmap/dns"
	"github.com/zmap/go-iptree/blacklist"
	"github.com/zmap/zdns/internal/util"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	googleDNSResolverAddr = "8.8.8.8:53"

	defaultTimeout               = 15 * time.Second // timeout for resolving a single name
	defaultIterativeTimeout      = 4 * time.Second  // timeout for single iteration in an iterative query
	defaultTransportMode         = UDPOrTCP
	defaultShouldRecycleSockets  = true
	defaultLogVerbosity          = 3 // 1 = lowest, 5 = highest
	defaultRetries               = 1
	defaultMaxDepth              = 10
	defaultCheckingDisabledBit   = false // Sends DNS packets with the CD bit set
	defaultNameServerModeEnabled = false // Treats input as nameservers to query with a static query rather than queries to send to a static name server
	defaultCacheSize             = 10000
	defaultShouldTrace           = false
	defaultDNSSECEnabled         = false
	defaultIPVersionMode         = IPv4OrIPv6
	defaultNameServerConfigFile  = "/etc/resolv.conf"
	defaultLookupAllNameServers  = false
)

// ResolverConfig is a struct that holds all the configuration options for a Resolver. It is used to create a new Resolver.
type ResolverConfig struct {
	Cache        *Cache
	CacheSize    int      // don't use both cache and cacheSize
	LookupClient Lookuper // either a functional or mock Lookuper client for testing

	Blacklist *blacklist.Blacklist
	// Wrap this with a SafeBlacklist
	BlMutex *sync.Mutex

	LocalAddr net.IP

	Retries     int
	ShouldTrace bool
	LogLevel    log.Level

	TransportMode        transportMode
	IPVersionMode        ipVersionMode
	ShouldRecycleSockets bool

	IsIterative          bool
	IterativeTimeout     time.Duration
	Timeout              time.Duration // timeout for the network conns
	MaxDepth             int
	NameServers          []string
	LookupAllNameServers bool

	DNSSecEnabled       bool
	EdnsOptions         []dns.EDNS0
	CheckingDisabledBit bool
}

func (rc *ResolverConfig) isValid() (bool, string) {
	if isValid, reason := rc.TransportMode.isValid(); !isValid {
		return false, reason
	}
	if isValid, reason := rc.IPVersionMode.isValid(); !isValid {
		return false, reason
	}
	if rc.Cache != nil && rc.CacheSize != 0 {
		return false, "cannot use both cache and cacheSize"
	}
	return true, ""
}

func NewResolverConfig() *ResolverConfig {
	c := new(Cache)
	c.Init(defaultCacheSize)
	return &ResolverConfig{
		LookupClient: LookupClient{},
		Cache:        c,

		Blacklist: blacklist.New(),
		BlMutex:   new(sync.Mutex),

		TransportMode:        defaultTransportMode,
		IPVersionMode:        defaultIPVersionMode,
		ShouldRecycleSockets: defaultShouldRecycleSockets,

		Retries:     defaultRetries,
		ShouldTrace: defaultShouldTrace,
		LogLevel:    defaultLogVerbosity,

		Timeout:          defaultTimeout,
		IterativeTimeout: defaultIterativeTimeout,
		MaxDepth:         defaultMaxDepth,

		DNSSecEnabled:       defaultDNSSECEnabled,
		CheckingDisabledBit: defaultCheckingDisabledBit,
	}
}

type Resolver struct {
	cache        *Cache
	lookupClient Lookuper // either a functional or mock Lookuper client for testing

	blacklist *blacklist.Blacklist
	blMutex   *sync.Mutex

	udpClient *dns.Client
	tcpClient *dns.Client
	conn      *dns.Conn
	localAddr net.IP

	retries     int
	shouldTrace bool
	logLevel    log.Level

	transportMode        transportMode
	ipVersionMode        ipVersionMode
	shouldRecycleSockets bool

	isIterative          bool // whether the user desires iterative resolution or recursive
	iterativeTimeout     time.Duration
	timeout              time.Duration // timeout for the network conns
	maxDepth             int
	nameServers          []string
	lookupAllNameServers bool

	dnsSecEnabled       bool
	ednsOptions         []dns.EDNS0
	checkingDisabledBit bool
}

func InitResolver(config *ResolverConfig) (*Resolver, error) {
	if isValid, notValidReason := config.isValid(); !isValid {
		return nil, fmt.Errorf("invalid resolver: %s", notValidReason)
	}
	var c *Cache
	if config.CacheSize != 0 {
		c = new(Cache)
		c.Init(config.CacheSize)
	} else if config.Cache != nil {
		c = config.Cache
	} else {
		c = new(Cache)
		c.Init(defaultCacheSize)
	}
	// copy relevent all values from config to resolver
	r := &Resolver{
		cache:        c,
		lookupClient: config.LookupClient,

		blacklist: config.Blacklist,
		blMutex:   config.BlMutex,

		localAddr: config.LocalAddr,

		retries:     config.Retries,
		shouldTrace: config.ShouldTrace,
		logLevel:    config.LogLevel,

		transportMode:        config.TransportMode,
		ipVersionMode:        config.IPVersionMode,
		shouldRecycleSockets: config.ShouldRecycleSockets,

		isIterative: config.IsIterative,
		timeout:     config.Timeout,
		nameServers: config.NameServers,

		dnsSecEnabled:       config.DNSSecEnabled,
		ednsOptions:         config.EdnsOptions,
		checkingDisabledBit: config.CheckingDisabledBit,
	}
	log.SetLevel(r.logLevel)
	if len(r.localAddr) == 0 {
		// localAddr not set, so we need to find the default IP address
		conn, err := net.Dial("udp", googleDNSResolverAddr)
		if err != nil {
			return nil, fmt.Errorf("unable to find default IP address to open socket: %w", err)
		}
		r.localAddr = conn.LocalAddr().(*net.UDPAddr).IP
		// cleanup socket
		if err = conn.Close(); err != nil {
			log.Error("unable to close test connection to Google public DNS: ", err)
		}
	}
	if r.shouldRecycleSockets {
		// create persistent connection
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: r.localAddr})
		if err != nil {
			return nil, fmt.Errorf("unable to create UDP connection: %w", err)
		}
		r.conn = new(dns.Conn)
		r.conn.Conn = conn
	}

	usingUDP := r.transportMode == UDPOrTCP || r.transportMode == UDPOnly
	if usingUDP {
		r.udpClient = new(dns.Client)
		r.udpClient.Timeout = r.timeout
		r.udpClient.Dialer = &net.Dialer{
			Timeout:   r.timeout,
			LocalAddr: &net.UDPAddr{IP: r.localAddr},
		}
	}
	usingTCP := r.transportMode == UDPOrTCP || r.transportMode == TCPOnly
	if usingTCP {
		r.tcpClient = new(dns.Client)
		r.tcpClient.Net = "tcp"
		r.tcpClient.Timeout = r.timeout
		r.tcpClient.Dialer = &net.Dialer{
			Timeout:   config.Timeout,
			LocalAddr: &net.TCPAddr{IP: r.localAddr},
		}
	}
	if r.isIterative {
		r.iterativeTimeout = config.IterativeTimeout
		r.maxDepth = config.MaxDepth
		r.lookupAllNameServers = config.LookupAllNameServers
		if r.nameServers == nil || len(r.nameServers) == 0 {
			// use the set of 13 root name servers
			r.nameServers = RootServers[:]
		}
	} else if r.nameServers == nil || len(r.nameServers) == 0 {
		// not iterative and client didn't specify name servers
		// configure the default name servers the OS is using
		ns, err := GetDNSServers(defaultNameServerConfigFile)
		if err != nil {
			ns = util.GetDefaultResolvers()
			log.Warn("Unable to parse resolvers file with error %w. Using ZDNS defaults: ", err, strings.Join(ns, ", "))
		}
		r.nameServers = ns
		log.Info("No name servers specified. will use: ", strings.Join(r.nameServers, ", "))
	}
	return r, nil
}

func (r *Resolver) Lookup(q *Question) (*Result, error) {
	var res interface{}
	var fullTrace Trace
	var status Status
	var err error

	ns := r.randomNameServer()
	if r.lookupAllNameServers {
		res, fullTrace, status, err = r.doLookupAllNameservers(*q, ns)
		if err != nil {
			return nil, fmt.Errorf("error resolving name %v for all name servers based on initial server %v: %w", q.Name, ns, err)
		}
	} else {
		res, fullTrace, status, err = r.lookupClient.DoSingleNameserverLookup(r, *q, ns)
		if err != nil {
			return nil, fmt.Errorf("error resolving name %v for a single name server %v: %w", q.Name, ns, err)
		}
	}
	return &Result{
		Data:   res,
		Trace:  fullTrace,
		Status: string(status),
	}, nil
}

// Close cleans up any resources used by the resolver. This should be called when the resolver is no longer needed.
// Lookup will panic if called after Close.
func (r *Resolver) Close() {
	if r.conn != nil {
		if err := r.conn.Close(); err != nil {
			log.Errorf("error closing connection: %v", err)
		}
	}
}

func (r *Resolver) randomNameServer() string {
	if r.nameServers == nil || len(r.nameServers) == 0 {
		log.Fatal("No name servers specified")
	}
	l := len(r.nameServers)
	if l == 0 {
		log.Fatal("No name servers specified")
	}
	return r.nameServers[rand.Intn(l)]
}

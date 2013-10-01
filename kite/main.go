package kite

import (
	"bufio"
	"code.google.com/p/go.net/websocket"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fatih/goset"
	"github.com/golang/groupcache"
	uuid "github.com/nu7hatch/gouuid"
	"io"
	"koding/db/models"
	"koding/newkite/balancer"
	"koding/newkite/peers"
	"koding/newkite/protocol"
	"log"
	"math"
	"net"
	"net/http"
	"net/rpc"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	kites       = peers.New()
	balance     = balancer.New()
	permissions = goset.New()
)

// Messenger is used to implement various Messaging patterns on top of the
// Kites.
type Messenger interface {
	// Send is makes a request to the endpoint and returns the response
	Send([]byte) []byte

	// Consumer is a subscriber/consumer that listens to the endpoint. Incoming
	// data should be handler via the function that is passed.
	Consume(func([]byte))
}

// Clients is an interface that encapsulates basic operations on incoming and connected clients.
type Clients interface {
	// Add inserts a new client into the storage.
	Add(c *client)

	// Get returns a new client that matches the c.Addr field
	Get(c *client) *client

	// Remove deletes the client that matches the c.Addr field
	Remove(c *client)

	// Size returns the total number of clients connected currently
	Size() int

	// List returns a slice of all clients
	List() []*client
}

/*
Kite defines a single process that enables distributed service messaging amongst
the peers it is connected. A Kite process acts as a Client and as a Server. That
means it can receive request, process them, but it also can make request to other
kites.

A Kite can be anything. It can be simple Image processing kite (which would
process data), it could be a Chat kite that enables peer-to-peer chat. For examples
we have FileSystem kite that expose the file system to a client.

A Kite has several attributes:
1. It's an RPC server with has (semi)support for codecs like:
JSON-RPC, DNODE and GOB
2. It's a GroupCache client and server, which enables distributed caching
and data sharing amongst the peers it is connected.
3. ZMQ messaging system, that allows to communicate with another ZMQ server.
4. Distribute requests in RoundRobin fashion.

It's still under work and many parts are constantly changing.

Following should be done later or soon:

1. Decide which functions of the Kite struct should be exported or not.
2. Make Groupcache work, method templates are written but need modification
and testing.
3. Implement a pluggable AUTH mechanism.
4. A better way to register functions to go's net/rpc. Something like:
k.Register("methodName", func() error)
5. MQ between peers. Kites should have Pub/Sub integrated that can message
with each other.
6. Monitoring data of the HOST. Capture stats like CPU, Memory, Load.
7. Limiter (or Firewall). Kite should have an Acess Control mechanism
for incoming requests, for certain thresholds (like CPU, Memory), for certain
kites, and so on.
8. Tests, tests, tests... we need Unit tests, Benchmark tests, and many other
things.
9. Web Dashboard for controlling kites, starting them, stopping them and many
other non-thinked things.
*/
type Kite struct {
	// user that calls/runs the kite
	Username string

	// Kitename defines the name that a kite is running on. This field is also used
	// for communicating with other kites with the same name.
	Kitename string

	// Uuid is a genereated unique id string that defines this Kite.
	Uuid string

	// RPC and GroupCache addresses, also expoxed to Kontrol
	Addr string

	// TODO: fill here
	PublicKey string

	// Hostname the kite is running on. Uses os.Hostname()
	Hostname string
	LocalIP  string // local network interface
	PublicIP string // public reachable IP

	// Port that the kite is going to be run.
	Port string

	// every kite should have version
	Version string

	// Registered is true if the Kite is registered to kontrol itself
	Registered bool

	// other kites that needs to be run, in order to run this one
	Dependencies string

	// by default yes, if disabled it bypasses kontrol
	KontrolEnabled bool

	// method map for shared methods
	Methods map[string]string

	// implements the Messenger interface
	Messenger Messenger

	// implements the Clients interface
	Clients Clients

	Pool   *groupcache.HTTPPool
	Group  *groupcache.Group
	Server *rpc.Server

	// used to start the rpc server only once
	OnceServer sync.Once

	// used when multiple goroutines are requesting information from kontrol
	// we only make on request to Kontrol.
	OnceCall sync.Once
}

// New creates, initialize and then returns a new Kite instance. It accept
// three  arguments. o is a config struct that needs to be filled with several
// informations like Name, Port, IP and so on. rcvr is a struct on which your
// exported method's are defined. methods is a map that expose your methods
// with different names to the outside.
func New(o *protocol.Options, rcvr interface{}, methods map[string]interface{}) *Kite {
	var err error
	if o == nil {
		o, err = readOptions("manifest.json")
		if err != nil {
			log.Fatal("error: could not read config file", err)
		}
	}

	// some simple validations for config
	if o.Username == "" || o.Kitename == "" {
		log.Fatal("error: options data is not set properly")
	}

	hostname, _ := os.Hostname()
	id, _ := uuid.NewV4()
	kiteID := id.String()

	publicKey, err := getKey("public")
	if err != nil {
		log.Fatal("public key reading:", err)
	}

	publicIP := getPublicIP(o.PublicIP)
	localIP := getLocalIP(o.LocalIP)

	port := o.Port
	if o.Port == "" {
		port = "0" // binds to an automatic port
	}

	// print dependencies
	// pwd, _ := os.Getwd()
	// getDeps(pwd, o.Kitename)

	k := &Kite{
		Username:       o.Username,
		Kitename:       o.Username + "/" + o.Kitename,
		Version:        o.Version,
		Uuid:           kiteID,
		PublicKey:      publicKey,
		Addr:           localIP + ":" + port,
		PublicIP:       publicIP,
		LocalIP:        localIP,
		Port:           port,
		Hostname:       hostname,
		Server:         rpc.NewServer(),
		KontrolEnabled: true,
		Methods:        createMethodMap(o.Kitename, rcvr, methods),
		Messenger:      NewZeroMQ(kiteID, o.Kitename, "all"),
		Clients:        NewClients(),
	}

	if rcvr != nil {
		k.AddFunction(o.Kitename, rcvr)
	}

	return k
}

// Start is a blocking method. It runs the kite server and then accepts requests
// asynchronously. It can be started in a goroutine if you wish to use kite as a
// client too.
func (k *Kite) Start() {
	// Start our blocking subscriber loop. We except messages in the format of:
	// filter:msg, where msg is in format JSON  of PubResponse protocol format.
	// Latter is important to ensure robustness, if not we have to unmarshal or
	// check every incoming message.
	if !k.KontrolEnabled {
		k.Registered = true
		k.serve(k.Addr)
	} else {
		k.Messenger.Consume(k.handle)

	}
}

// handle is a method that interprets the incoming message from Kontrol. The
// incoming message is in form of protocol.PubResponse.
func (k *Kite) handle(msg []byte) {
	var r protocol.PubResponse
	err := json.Unmarshal(msg, &r)
	if err != nil {
		log.Println(err)
		return
	}

	// treat any incoming data as a ping, don't just rely on ping command
	// this makes the kite more robust if we can't catch one of the pings.
	k.Pong()

	switch r.Action {
	case protocol.AddKite:
		k.AddKite(r)
	case protocol.RemoveKite:
		k.RemoveKite(r)
	case protocol.UpdateKite:
		k.Registered = false //trigger reinitialization
	case "ping":
		// This is needed for Node Coordination, that means we register ourself
		// only if we got an "hello" from one of the kontrol servers. This is
		// needed in order to catch all PUB messages from Kontrol. For more
		// information about this pattern read "Node Coordination" from the Zmq
		// Guide.
		k.InitializeKite()
	default:
		return
	}

}

// AddKite is executed when a protocol.AddKite message has been received
// trough the handler.
func (k *Kite) AddKite(r protocol.PubResponse) {
	if !k.Registered {
		return
	}

	kite := &models.Kite{
		Base: protocol.Base{
			Username: r.Username,
			Kitename: r.Kitename,
			Version:  r.Version,
			Uuid:     r.Uuid,
			Hostname: r.Hostname,
			Addr:     r.Addr,
		},
	}

	kites.Add(kite)
	k.SetPeers(k.PeersAddr()...)

	debug("[%s] -> known peers -> %v\n", r.Action, k.PeersAddr())
}

// RemoveKite is executed when a protocol.AddKite message has been received
// trough the handler.
func (k *Kite) RemoveKite(r protocol.PubResponse) {
	if !k.Registered {
		return
	}

	kites.Remove(r.Uuid)
	debug("[%s] -> known peers -> %v\n", r.Action, k.PeersAddr())
}

// Pong sends a 'pong' message whenever the kite receives a message from Kontrol.
// This is used for node coordination and notifier Kontrol that the Kite is alive.
func (k *Kite) Pong() {
	m := protocol.Request{
		Base: protocol.Base{
			Kitename: k.Kitename,
			Uuid:     k.Uuid,
		},
		Action: "pong",
	}

	msg, _ := json.Marshal(&m)

	resp := k.Messenger.Send(msg)
	if string(resp) == "UPDATE" {
		k.Registered = false
	}
}

// InitializeKite runs the builtin RPC server and also registers itself to Kontrol
// when the kite.KontrolEnabled flag is enabled. This method is non-blocking.
func (k *Kite) InitializeKite() {
	if k.Registered {
		return
	}

	debug("not registered, sending register request to kontrol...")
	err := k.RegisterToKontrol()
	if err != nil {
		fmt.Println(err)
		return
	}

	onceBody := func() { k.serve(k.Addr) }
	go k.OnceServer.Do(onceBody)

	k.Registered = true
}

// RegisterToKontrol sends a register message to Kontrol. It returns an error
// when it is not allowed by Kontrol. If allowed, nil is returned.
func (k *Kite) RegisterToKontrol() error {
	// Wait until the servers are ready
	m := protocol.Request{
		Base: protocol.Base{
			Username:  k.Username,
			Kitename:  k.Kitename,
			Version:   k.Version,
			Uuid:      k.Uuid,
			PublicKey: k.PublicKey,
			Hostname:  k.Hostname,
			// Addr:      k.PublicIP + ":" + k.Port,
			Addr:     k.Addr,
			LocalIP:  k.LocalIP,
			PublicIP: k.PublicIP,
			Port:     k.Port,
		},
		Action: "register",
	}

	msg, err := json.Marshal(&m)
	if err != nil {
		log.Println("kontrolRequest marshall err", err)
		return err
	}

	result := k.Messenger.Send(msg)
	var resp protocol.RegisterResponse
	err = json.Unmarshal(result, &resp)
	if err != nil {
		return err
	}

	switch resp.Result {
	case protocol.AllowKite:
		fmt.Printf("registered to kontrol: \n  Addr\t\t: %s\n  Version\t: %s\n  Uuid\t\t: %s\n\n", k.Addr, k.Version, k.Uuid)
		return nil
	case protocol.PermitKite:
		return errors.New("no permission to run")
	default:
		return errors.New("got a nonstandard response")
	}

	return nil
}

/******************************************

RPC

******************************************/

// Can connect to RPC service using HTTP CONNECT to rpcPath.
var connected = "200 Connected to Go RPC"

// dialClient is used to connect to a Remote Kite via the GOB codec. This is
// used by other external kite methods.
func (k *Kite) dialClient(kite *models.Kite) (*rpc.Client, error) {
	debug("establishing HTTP client conn for %s - %s on %s\n", kite.Kitename, kite.Addr, kite.Hostname)
	var err error
	conn, err := net.Dial("tcp4", kite.Addr)
	if err != nil {
		return nil, err
	}
	io.WriteString(conn, "CONNECT "+rpc.DefaultRPCPath+" HTTP/1.0\n\n")

	// Require successful HTTP response
	// before switching to RPC protocol.
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && resp.Status == connected {
		c := NewKiteClientCodec(k, conn) // pass our custom codec
		return rpc.NewClientWithCodec(c), nil
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + resp.Status)
	}
	conn.Close()
	return nil, &net.OpError{
		Op:   "dial-http",
		Net:  "tcp " + kite.Addr,
		Addr: nil,
		Err:  err,
	}
}

// serve starts our rpc server with the given addr. Addr should be in form of
// "ip:port"
func (k *Kite) serve(addr string) {
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		log.Println("PANIC!!!!! RPC SERVER COULD NOT INITIALIZED:", err)
		os.Exit(1)
		return
	}

	k.Addr = listener.Addr().String()
	fmt.Println("serve addr is", k.Addr)

	// GroupCache
	k.NewPool(k.Addr)
	k.NewGroup()

	k.Server.HandleHTTP(rpc.DefaultRPCPath, rpc.DefaultDebugPath)
	http.Serve(listener, k)
}

// ServeHTTP interface for http package.
func (k *Kite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == protocol.WEBSOCKET_PATH {
		websocket.Handler(k.serveWS).ServeHTTP(w, r)
		return
	}

	debug("a new rpc call is done from", r.RemoteAddr)
	if r.Method != "CONNECT" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		io.WriteString(w, "405 must CONNECT\n")
		return
	}

	debug("hijacking conn")
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		log.Print("rpc hijacking ", r.RemoteAddr, ": ", err.Error())
		return
	}

	io.WriteString(conn, "HTTP/1.0 "+connected+"\n\n")
	k.Server.ServeCodec(NewKiteServerCodec(k, conn))

}

// serveWS is used serving content over WebSocket. Is used internally via
// ServeHTTP method.
func (k *Kite) serveWS(ws *websocket.Conn) {
	addr := ws.Request().RemoteAddr
	fmt.Printf("[%s] client connected\n", addr)

	k.Clients.Add(&client{Conn: ws, Addr: addr})

	// k.Server.ServeCodec(NewJsonServerCodec(k, ws))
	k.Server.ServeCodec(NewDnodeServerCodec(k, ws))
}

// AddFunction is used to add new structs with exposed methods with a different
// name.
func (k *Kite) AddFunction(name string, method interface{}) {
	k.Server.RegisterName(name, method)
}

// CallSync makes a blocking request to another kite. Kite should be in form of
// "username/kitename", method should be known ahead of time. args and result is
// used by the remote kite, therefore you should know what the kite is expecting.
func (k *Kite) CallSync(kite, method string, args interface{}, result interface{}) error {
	remoteKite, err := k.getRemoteKite(kite)
	if err != nil {
		return err
	}

	rpcFunc := kite + "." + method
	err = remoteKite.Client.Call(rpcFunc, args, result)
	if err != nil {
		log.Println(err)
		return fmt.Errorf("[%s] call error: %s", kite, err.Error())
	}

	return nil
}

// Call makes a non-blocking request to another kite. Kite should be in form of
// "username/kitename", the method should be known ahead of time. args is
// used by the remote kite, therefore you should know what the kite is expecting.
// fn is a callback that is executed when the result and error has been received.
// Currently only string as a result is supported, but it needs to be changed.
func (k *Kite) Call(kite, method string, args interface{}, fn func(err error, res string)) *rpc.Call {
	rpcFunc := kite + "." + method
	ticker := time.NewTicker(time.Second * 1)
	runCall := make(chan bool, 1)
	resetOnce := make(chan bool, 1)

	var remoteKite *models.Kite
	var err error

	for {
		select {
		case <-ticker.C:
			remoteKite, err = k.getRemoteKite(kite)
			if err != nil {
				debug("no remote kites available, requesting some ...")
				m := protocol.Request{
					Base: protocol.Base{
						Username: k.Username,
						Kitename: k.Kitename,
						Version:  k.Version,
						Uuid:     k.Uuid,
						Hostname: k.Hostname,
						Addr:     k.Addr,
					},
					RemoteKite: kite,
					Action:     "getKites",
				}

				msg, err := json.Marshal(&m)
				if err != nil {
					log.Println("kontrolRequest marshall err", err)
					continue
				}

				onceBody := func() {
					debug("sending requesting message...")
					k.Messenger.Send(msg)
				}

				k.OnceCall.Do(onceBody) // to prevent multiple get request when called concurrently
			} else {
				ticker.Stop()
				debug("making rpc call to '%s' with token '%s': -> ", remoteKite.Kitename, remoteKite.Token)
				runCall <- true
				resetOnce <- true
			}
		case <-runCall:
			var result string

			a := &protocol.KiteRequest{
				Base: protocol.Base{
					Username: k.Username,
					Kitename: k.Kitename,
					Version:  k.Version,
					Token:    remoteKite.Token,
					Uuid:     k.Uuid,
					Hostname: k.Hostname,
				},
				Args:   args,
				Origin: protocol.ORIGIN_GOB,
			}

			d := remoteKite.Client.Go(rpcFunc, a, &result, nil)

			select {
			case <-d.Done:
				fn(d.Error, result)
			case <-time.Tick(10 * time.Second):
				fn(d.Error, result)
			}
			return d
		case <-resetOnce:
			k.OnceCall = sync.Once{}
		}
	}
}

func (k *Kite) getRemoteKite(kite string) (*models.Kite, error) {
	r, err := k.roundRobin(kite)
	if err != nil {
		return nil, err
	}

	if r.Client == nil {
		var err error
		r.Client, err = k.dialClient(r)
		if err != nil {
			return nil, err
		}
		kites.Add(r)
	}

	return r, nil
}

func (k *Kite) roundRobin(kite string) (*models.Kite, error) {
	// TODO: use container/ring :)
	remoteKites := k.RemoteKites(kite)
	lenOfKites := len(remoteKites)
	if lenOfKites == 0 {
		return nil, fmt.Errorf("kite %s does not exist", kite)
	}

	index := balance.GetIndex(kite)
	N := float64(lenOfKites)
	n := int(math.Mod(float64(index+1), N))
	balance.AddOrUpdateIndex(kite, n)
	return remoteKites[n], nil
}

func (k *Kite) RemoteKites(kite string) []*models.Kite {
	l := kites.List()
	remoteKites := make([]*models.Kite, 0, len(l)-1) // allocate one less, it's the kite itself

	for _, r := range l {
		if r.Kitename == kite {
			remoteKites = append(remoteKites, r)
		}
	}

	return remoteKites
}

/******************************************

GroupCache

******************************************/
func (k *Kite) NewPool(addr string) {
	k.Pool = groupcache.NewHTTPPool(addr)
}

func (k *Kite) NewGroup() {
	k.Group = groupcache.NewGroup(k.Kitename, 64<<20, groupcache.GetterFunc(
		func(ctx groupcache.Context, key string, dest groupcache.Sink) error {
			dest.SetString("fatih")
			return nil
		}))
}

func (k *Kite) GetString(name, key string) (result string) {
	if k.Group == nil {
		return
	}

	k.Group.Get(nil, key, groupcache.StringSink(&result))
	return
}

func (k *Kite) GetByte(name, key string) (result []byte) {
	if k.Group == nil {
		return
	}

	k.Group.Get(nil, key, groupcache.AllocatingByteSliceSink(&result))
	return
}

func (k *Kite) SetPeers(peers ...string) {
	k.Pool.Set(peers...)
}

func (k *Kite) PeersAddr() []string {
	list := make([]string, 0)
	for _, kite := range kites.List() {
		list = append(list, kite.Addr)
	}
	return list
}

/******************************************

Misc

******************************************/

func (k *Kite) Broadcast(msg string) {
	clients := k.Clients.List()
	for _, client := range clients {
		go func() {
			if err := websocket.Message.Send(client.Conn, msg); err != nil {
				fmt.Println("Could not send message to ", client.Addr, err.Error())
			}
		}()
	}
}

func createMethodMap(kitename string, rcvr interface{}, methods map[string]interface{}) map[string]string {
	funcName := func(i interface{}) string {
		return runtime.FuncForPC(reflect.ValueOf(i).Pointer()).Name()
	}

	t := reflect.TypeOf(rcvr)
	structName := strings.TrimPrefix(t.String(), "*")

	methodMap := make(map[string]string)
	for name, method := range methods {
		methodMap[name] = kitename + "." + strings.TrimPrefix(funcName(method), structName+".")
	}

	return methodMap
}

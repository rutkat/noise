package network

import (
	"bufio"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/perlin-network/noise/crypto"
	"github.com/perlin-network/noise/network/transport"
	"github.com/perlin-network/noise/peer"
	"github.com/perlin-network/noise/protobuf"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/golang/glog"
	"github.com/pkg/errors"
)

const (
	defaultConnectionTimeout = 60 * time.Second
	defaultReceiveWindowSize = 4096
	defaultSendWindowSize    = 4096
	defaultWriteBufferSize   = 4096
	defaultWriteFlushLatency = 50 * time.Millisecond
	defaultWriteTimeout      = 3 * time.Second
)

var contextPool = sync.Pool{
	New: func() interface{} {
		return new(PluginContext)
	},
}

var (
	_ NetworkInterface = (*Network)(nil)
)

// Network represents the current networking state for this node.
type Network struct {
	opts options

	// Node's keypair.
	keys *crypto.KeyPair

	// Full address to listen on. `protocol://host:port`
	Address string

	// Map of plugins registered to the network.
	// map[string]Plugin
	Plugins *PluginList

	// Node's cryptographic ID.
	ID peer.ID

	// Map of connection addresses (string) <-> *network.PeerClient
	// so that the Network doesn't dial multiple times to the same ip
	Peers *sync.Map

	//RecvQueue chan *protobuf.Message

	// Map of connection addresses (string) <-> *ConnState
	Connections *sync.Map

	// Map of protocol addresses (string) <-> *transport.Layer
	Transports *sync.Map

	// <-Listening will block a goroutine until this node is listening for peers.
	Listening chan struct{}

	// <-kill will begin the server shutdown process
	kill chan struct{}
}

// options for network struct
type options struct {
	connectionTimeout time.Duration
	signaturePolicy   crypto.SignaturePolicy
	hashPolicy        crypto.HashPolicy
	recvWindowSize    int
	sendWindowSize    int
	writeBufferSize   int
	writeFlushLatency time.Duration
	writeTimeout      time.Duration
}

type ConnState struct {
	conn         net.Conn
	writer       *bufio.Writer
	messageNonce uint64
	writerMutex  *sync.Mutex
}

// Init starts all network I/O workers.
func (n *Network) Init() {
	// Spawn write flusher.
	go n.flushLoop()
}

func (n *Network) flushLoop() {
	t := time.NewTicker(n.opts.writeFlushLatency)
	defer t.Stop()
	for {
		select {
		case <-n.kill:
			return
		case <-t.C:
			n.Connections.Range(func(key, value interface{}) bool {
				if state, ok := value.(*ConnState); ok {
					state.writerMutex.Lock()
					if err := state.writer.Flush(); err != nil {
						glog.Warning(err)
					}
					state.writerMutex.Unlock()
				}
				return true
			})
		}
	}
}

// GetKeys returns the keypair for this network
func (n *Network) GetKeys() *crypto.KeyPair {
	return n.keys
}

func (n *Network) dispatchMessage(client *PeerClient, msg *protobuf.Message) {
	// Check if the client is ready.
	if !client.IncomingReady() {
		return
	}
	var ptr types.DynamicAny
	if err := types.UnmarshalAny(msg.Message, &ptr); err != nil {
		glog.Error(err)
		return
	}

	if msg.RequestNonce > 0 && msg.ReplyFlag {
		if _state, exists := client.Requests.Load(msg.RequestNonce); exists {
			state := _state.(*RequestState)
			select {
			case state.data <- ptr.Message:
			case <-state.closeSignal:
			}
			return
		}
	}

	switch msgRaw := ptr.Message.(type) {
	case *protobuf.Bytes:
		client.handleBytes(msgRaw.Data)
	default:
		ctx := contextPool.Get().(*PluginContext)
		ctx.client = client
		ctx.message = msgRaw
		ctx.nonce = msg.RequestNonce

		go func() {
			// Execute 'on receive message' callback for all plugins.
			n.Plugins.Each(func(plugin PluginInterface) {
				if err := plugin.Receive(ctx); err != nil {
					glog.Errorf("%+v", err)
				}
			})

			contextPool.Put(ctx)
		}()
	}
}

// Listen starts listening for peers on a port.
func (n *Network) Listen() {

	// Handle 'network starts listening' callback for plugins.
	n.Plugins.Each(func(plugin PluginInterface) {
		plugin.Startup(n)
	})

	// Handle 'network stops listening' callback for plugins.
	defer func() {
		n.Plugins.Each(func(plugin PluginInterface) {
			plugin.Cleanup(n)
		})
	}()

	addrInfo, err := ParseAddress(n.Address)
	if err != nil {
		glog.Fatal(err)
	}

	var listener net.Listener

	if t, exists := n.Transports.Load(addrInfo.Protocol); exists {
		listener, err = t.(transport.Layer).Listen(int(addrInfo.Port))
		if err != nil {
			glog.Fatal(err)
		}
	} else {
		glog.Fatal("invalid protocol: " + addrInfo.Protocol)
	}

	close(n.Listening)

	glog.Infof("Listening for peers on %s.\n", n.Address)

	// handle server shutdowns
	go func() {
		select {
		case <-n.kill:
			// cause listener.Accept() to stop blocking so it can continue the loop
			listener.Close()
		}
	}()

	// Handle new clients.
	for {
		if conn, err := listener.Accept(); err == nil {
			go n.Accept(conn)

		} else {
			// if the Shutdown flag is set, no need to continue with the for loop
			select {
			case <-n.kill:
				glog.Infof("Shutting down server on %s.\n", n.Address)
				return
			default:
				glog.Error(err)
			}
		}
	}
}

// Client either creates or returns a cached peer client given its host address.
func (n *Network) Client(address string) (*PeerClient, error) {
	address, err := ToUnifiedAddress(address)
	if err != nil {
		return nil, err
	}

	if address == n.Address {
		return nil, errors.New("network: peer should not dial itself")
	}

	clientNew, err := createPeerClient(n, address)
	if err != nil {
		return nil, err
	}

	c, exists := n.Peers.LoadOrStore(address, clientNew)
	if exists {
		client := c.(*PeerClient)

		if !client.OutgoingReady() {
			return nil, errors.New("network: peer failed to connect")
		}

		return client, nil
	}

	client := c.(*PeerClient)
	defer func() {
		close(client.outgoingReady)
	}()

	conn, err := n.Dial(address)
	if err != nil {
		n.Peers.Delete(address)
		return nil, err
	}

	n.Connections.Store(address, &ConnState{
		conn:        conn,
		writer:      bufio.NewWriterSize(conn, n.opts.writeBufferSize),
		writerMutex: new(sync.Mutex),
	})

	client.Init()

	return client, nil
}

// BlockUntilListening blocks until this node is listening for new peers.
func (n *Network) BlockUntilListening() {
	<-n.Listening
}

// Bootstrap with a number of peers and commence a handshake.
func (n *Network) Bootstrap(addresses ...string) {
	n.BlockUntilListening()

	addresses = FilterPeers(n.Address, addresses)

	for _, address := range addresses {
		client, err := n.Client(address)

		if err != nil {
			glog.Error(err)
			continue
		}

		err = client.Tell(&protobuf.Ping{})
		if err != nil {
			continue
		}
	}
}

// Dial establishes a bidirectional connection to an address, and additionally handshakes with said address.
func (n *Network) Dial(address string) (net.Conn, error) {
	addrInfo, err := ParseAddress(address)
	if err != nil {
		return nil, err
	}

	if addrInfo.Host != "127.0.0.1" {
		host, err := ParseAddress(n.Address)
		if err != nil {
			return nil, err
		}
		// check if dialing address is same as its own IP
		if addrInfo.Host == host.Host {
			addrInfo.Host = "127.0.0.1"
		}
	}

	// Choose scheme.
	t, exists := n.Transports.Load(addrInfo.Protocol)
	if !exists {
		glog.Fatal("invalid protocol: " + addrInfo.Protocol)
	}

	var conn net.Conn
	conn, err = t.(transport.Layer).Dial(addrInfo.HostPort())
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Accept handles peer registration and processes incoming message streams.
func (n *Network) Accept(incoming net.Conn) {
	var outgoing net.Conn

	var client *PeerClient
	var clientInit sync.Once

	// Cleanup connections when we are done with them.
	defer func() {
		if client != nil {
			client.Close()
		}

		if incoming != nil {
			incoming.Close()
		}

		if outgoing != nil {
			outgoing.Close()
		}
	}()

	for {
		msg, err := n.receiveMessage(incoming)
		if err != nil {
			if err != errEmptyMsg {
				glog.Error(err)
			}
			break
		}

		go func() {
			// Initialize client if not exists.
			clientInit.Do(func() {
				client, err = n.Client(msg.Sender.Address)
				if err != nil {
					return
				}

				client.ID = (*peer.ID)(msg.Sender)

				// Load an outgoing connection.
				if state, established := n.Connections.Load(client.ID.Address); established {
					outgoing = state.(*ConnState).conn
				} else {
					err = errors.New("network: failed to load session")
				}

				// Signal that the client is ready.
				close(client.incomingReady)
			})

			if err != nil {
				glog.Error(err)
				return
			}

			// Peer sent message with a completely different ID. Disconnect.
			if !client.ID.Equals(peer.ID(*msg.Sender)) {
				glog.Errorf("message signed by peer %s but client is %s", peer.ID(*msg.Sender), client.ID.Address)
				return
			}

			client.Submit(func() { n.dispatchMessage(client, msg) })
		}()
	}
}

// Plugin returns a plugins proxy interface should it be registered with the
// network. The second returning parameter is false otherwise.
//
// Example: network.Plugin((*Plugin)(nil))
func (n *Network) Plugin(key interface{}) (PluginInterface, bool) {
	return n.Plugins.Get(key)
}

// PrepareMessage marshals a message into a *protobuf.Message and signs it with this
// nodes private key. Errors if the message is null.
func (n *Network) PrepareMessage(message proto.Message) (*protobuf.Message, error) {
	if message == nil {
		return nil, errors.New("network: message is null")
	}

	raw, err := types.MarshalAny(message)
	if err != nil {
		return nil, err
	}

	id := protobuf.ID(n.ID)

	signature, err := n.keys.Sign(
		n.opts.signaturePolicy,
		n.opts.hashPolicy,
		SerializeMessage(&id, raw.Value),
	)
	if err != nil {
		return nil, err
	}

	msg := &protobuf.Message{
		Message:   raw,
		Sender:    &id,
		Signature: signature,
	}
	return msg, nil
}

// Write asynchronously sends a message to a denoted target address.
func (n *Network) Write(address string, message *protobuf.Message) error {
	s, exists := n.Connections.Load(address)
	if !exists {
		return errors.New("network: connection does not exist")
	}
	state := s.(*ConnState)

	message.MessageNonce = atomic.AddUint64(&state.messageNonce, 1)

	state.conn.SetWriteDeadline(time.Now().Add(n.opts.writeTimeout))

	err := n.sendMessage(state.writer, message, state.writerMutex)
	if err != nil {
		return err
	}
	return nil
}

// Broadcast asynchronously broadcasts a message to all peer clients.
func (n *Network) Broadcast(message proto.Message) {
	n.Peers.Range(func(key, value interface{}) bool {
		client := value.(*PeerClient)

		err := client.Tell(message)
		if err != nil {
			glog.Warningf("failed to send message to peer %v [err=%s]", client.ID, err)
		}

		return true
	})
}

// BroadcastByAddresses broadcasts a message to a set of peer clients denoted by their addresses.
func (n *Network) BroadcastByAddresses(message proto.Message, addresses ...string) {
	signed, err := n.PrepareMessage(message)
	if err != nil {
		return
	}

	for _, address := range addresses {
		n.Write(address, signed)
	}
}

// BroadcastByIDs broadcasts a message to a set of peer clients denoted by their peer IDs.
func (n *Network) BroadcastByIDs(message proto.Message, ids ...peer.ID) {
	signed, err := n.PrepareMessage(message)
	if err != nil {
		return
	}

	for _, id := range ids {
		n.Write(id.Address, signed)
	}
}

// BroadcastRandomly asynchronously broadcasts a message to random selected K peers.
// Does not guarantee broadcasting to exactly K peers.
func (n *Network) BroadcastRandomly(message proto.Message, K int) {
	var addresses []string

	n.Peers.Range(func(key, value interface{}) bool {
		client := value.(*PeerClient)

		addresses = append(addresses, client.Address)

		// Limit total amount of addresses in case we have a lot of peers.
		return len(addresses) <= K*3
	})

	// Flip a coin and shuffle :).
	rand.Shuffle(len(addresses), func(i, j int) {
		addresses[i], addresses[j] = addresses[j], addresses[i]
	})

	if len(addresses) < K {
		K = len(addresses)
	}

	n.BroadcastByAddresses(message, addresses[:K]...)
}

// Close shuts down the entire network.
func (n *Network) Close() {
	// Kill the listener.
	close(n.kill)

	// Clean out client connections.
	n.Peers.Range(func(key, value interface{}) bool {
		value.(*PeerClient).Close()
		return true
	})
}

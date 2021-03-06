package test

import (
	"testing"
	"time"

	"github.com/perlin-network/noise/crypto"
	"github.com/perlin-network/noise/crypto/blake2b"
	"github.com/perlin-network/noise/crypto/ed25519"
	"github.com/perlin-network/noise/network"
	"github.com/perlin-network/noise/network/discovery"
	"github.com/perlin-network/noise/peer"
	"github.com/perlin-network/noise/test/protobuf"
	"github.com/pkg/errors"
)

type env struct {
	name      string
	network   string // network type (e.g., tcp or kcp)
	hash      crypto.HashPolicy
	signature crypto.SignaturePolicy
}

var (
	kcpEnv  = env{name: "kcp-blake2b-ed25519", network: "kcp", hash: blake2b.New(), signature: ed25519.New()}
	tcpEnv  = env{name: "tcp-blake2b-ed25519", network: "tcp", hash: blake2b.New(), signature: ed25519.New()}
	allEnvs = []env{kcpEnv, tcpEnv}
)

type test struct {
	t *testing.T
	e env

	builderOptions []network.BuilderOption
	bootstrapNode  *network.Network
	nodes          []*network.Network
	plugins        []*network.Plugin
}

func (te *test) startBoostrap(numNodes int, plugins ...network.PluginInterface) {
	for i := 0; i < numNodes; i++ {
		builder := network.NewBuilderWithOptions(te.builderOptions...)
		builder.SetKeys(te.e.signature.RandomKeyPair())
		builder.SetAddress(network.FormatAddress(te.e.network, "localhost", uint16(network.GetRandomUnusedPort())))

		builder.AddPlugin(new(discovery.Plugin))
		builder.AddPlugin(new(MailBoxPlugin))

		for _, plugin := range plugins {
			builder.AddPlugin(plugin)
		}

		node, err := builder.Build()
		if err != nil {
			te.t.Fatalf("Build() = expected no error, got %v", err)
		}

		go node.Listen()

		if i == 0 {
			te.bootstrapNode = node
			node.BlockUntilListening()
		} else {
			te.nodes = append(te.nodes, node)
		}
	}

	for _, node := range te.nodes {
		node.Bootstrap(te.bootstrapNode.Address)
	}

	// wait for nodes to discover other peers
	for _, node := range te.nodes {
		pluginInt, ok := node.Plugin(discovery.PluginID)
		if !ok {
			te.t.Fatalf("Plugin() expected true, got false")
		}
		plugin := pluginInt.(*discovery.Plugin)
		routes := plugin.Routes
		peers := routes.GetPeers()
		for len(peers) < numNodes-1 {
			peers = routes.GetPeers()
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func (te *test) tearDown() {
	for _, node := range te.nodes {
		node.Close()
	}
	te.bootstrapNode.Close()
}

func (te *test) getMailbox(n *network.Network) *MailBoxPlugin {
	if n != nil {
		pluginInt, ok := n.Plugin(mailboxPluginID)
		if !ok {
			te.t.Errorf("Plugin(mailboxPluginID) expected true, got false")
		}
		return pluginInt.(*MailBoxPlugin)
	}
	return nil
}

func newTest(t *testing.T, e env, opts ...network.BuilderOption) *test {
	te := &test{
		t:              t,
		e:              e,
		builderOptions: opts,
	}
	return te
}

func getPeers(n *network.Network) ([]peer.ID, error) {
	pluginInt, ok := n.Plugin(discovery.PluginID)
	if !ok {
		return []peer.ID{}, errors.New("Plugin() expected true, got false")
	}
	plugin := pluginInt.(*discovery.Plugin)
	routes := plugin.Routes
	return routes.GetPeers(), nil
}

func TestNodeConnect(t *testing.T) {
	t.Parallel()

	for _, e := range allEnvs {
		testNodeConnect(t, e)
	}
}

func testNodeConnect(t *testing.T, e env) {
	te := newTest(t, e)
	numNodes := 10
	te.startBoostrap(numNodes)
	defer te.tearDown()

	pluginInt, ok := te.bootstrapNode.Plugin(discovery.PluginID)
	if !ok {
		t.Errorf("Plugin() expected true, got false")
	}
	plugin := pluginInt.(*discovery.Plugin)
	routes := plugin.Routes
	peers := routes.GetPeers()
	if len(peers) != numNodes-1 {
		t.Errorf("len(peers) = %d, want %d", len(peers), numNodes-1)
	}
}

func TestNodeBroadcast(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skipf("skipping %s in short mode", t.Name())
	}

	for _, e := range allEnvs {
		testNodeBroadcast(t, e)
	}
}

func testNodeBroadcast(t *testing.T, e env) {
	te := newTest(t, e, network.WriteTimeout(1*time.Second))
	numNodes := 3
	te.startBoostrap(numNodes)
	defer te.tearDown()

	expected := "test message"
	te.bootstrapNode.Broadcast(&protobuf.TestMessage{Message: expected})

	// Check if message was received by other nodes.
	for i, node := range te.nodes {
		select {
		case received := <-te.getMailbox(node).RecvMailbox:
			if received.Message != expected {
				t.Errorf("Expected message %s to be received by node %d but got %v\n", expected, i+1, received.Message)
			} else {
				t.Logf("Node %d received a message from Node 0.\n", i+1)
			}
		case <-time.After(100 * time.Millisecond):
			// FIXME(jack0): this can trigger sometimes, flaky
			t.Errorf("Timed out attempting to receive message from Node 0.\n")
		}
	}
}

/*
FIXME(jack0): something wrong with the sending, might be related to other PR
func TestNodeBroadcastByIDs(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skipf("skipping %s in short mode", t.Name())
	}

	for _, e := range allEnvs {
		testNodeBroadcastByIDs(t, e)
	}
}

func testNodeBroadcastByIDs(t *testing.T, e env) {
	te := newTest(t, e, network.WriteTimeout(1*time.Second))
	numNodes := 5
	te.startBoostrap(numNodes)
	defer te.tearDown()

	expected := "test message"
	peers, err := getPeers(te.bootstrapNode)
	if err != nil {
		t.Errorf("getPeers() = %v, expected <nil>", err)
	}

	numPeers := 2
	te.bootstrapNode.BroadcastByIDs(&protobuf.TestMessage{Message: expected}, peers[:numPeers]...)

	// Check if message was received by broadcasted peers.
	for i, node := range te.nodes {
		numMsgs := len(te.getMailbox(node).RecvMailbox)
		if i < numPeers {
			if numMsgs != 1 {
				t.Errorf("node %d got %d messages, expected 1", i+1, numMsgs)
			}
		} else {
			if numMsgs != 0 {
				t.Errorf("node %d got %d messages, expected 0", i+1, numMsgs)
			}
		}
	}
}

func TestNodeBroadcastByAddresses(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skipf("skipping %s in short mode", t.Name())
	}

	for _, e := range allEnvs {
		testNodeBroadcastByAddresses(t, e)
	}
}

func testNodeBroadcastByAddresses(t *testing.T, e env) {
	te := newTest(t, e, network.WriteTimeout(1*time.Second))
	numNodes := 5
	te.startBoostrap(numNodes)
	defer te.tearDown()

	expected := "test message"
	peers, err := getPeers(te.bootstrapNode)
	if err != nil {
		t.Errorf("getPeers() = %v, expected <nil>", err)
	}
	if len(peers) != 4 {
		t.Errorf("len(peers) = %d, expected 4", len(peers))
	}

	numPeers := 2
	addresses := []string{}
	for i := 0; i < numPeers; i++ {
		addresses = append(addresses, peers[i].Address)
	}
	te.bootstrapNode.BroadcastByAddresses(&protobuf.TestMessage{Message: expected}, addresses...)

	// Check if message was received by nodes receiving the broadcast.
	for i, node := range te.nodes {
		numMsgs := len(te.getMailbox(node).RecvMailbox)
		t.Logf("addresses: %+v address: %s i: %d\n", addresses, node.Address, i+1)
		for _, address := range addresses {
			if address == node.Address {
				if numMsgs != 1 {
					t.Errorf("node %d got %d messages, expected 1", i+1, numMsgs)
				}
			} else if numMsgs != 0 {
				t.Errorf("node %d got %d messages, expected 0", i+1, numMsgs)
			}
		}
	}
}
*/

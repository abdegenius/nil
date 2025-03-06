package network

import (
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/peer"
)

type PeerID = peer.ID

type Config struct {
	PrivateKey PrivateKey `yaml:"-"`

	KeysPath string `yaml:"keysPath,omitempty"`

	Prefix      string `yaml:"prefix,omitempty"`
	IPV4Address string `yaml:"ipv4,omitempty"`
	TcpPort     int    `yaml:"tcpPort,omitempty"`
	QuicPort    int    `yaml:"quicPort,omitempty"`

	Relay bool `yaml:"relay,omitempty"`

	DHTEnabled        bool          `yaml:"dhtEnabled,omitempty"`
	DHTBootstrapPeers AddrInfoSlice `yaml:"dhtBootstrapPeers,omitempty"`
	DHTMode           dht.ModeOpt   `yaml:"-,omitempty"`
}

func NewDefaultConfig() *Config {
	return &Config{
		KeysPath: "network-keys.yaml",
		DHTMode:  dht.ModeAutoServer,
		Prefix:   "/nil",
	}
}

func (c *Config) Enabled() bool {
	return c.TcpPort != 0 || c.QuicPort != 0
}

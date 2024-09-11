package viamrtsp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.viam.com/rdk/logging"
)

const (
	defaultMulticastAddress = "239.255.255.250:3702"
)

// RTSPDiscovery is responsible for discovering RTSP camera devices using WS-Discovery and ONVIF.
type RTSPDiscovery struct {
	multicastAddress string
	logger           logging.Logger
}

// NewRTSPDiscovery creates a new RTSPDiscovery instance with default values.
func NewRTSPDiscovery(multicastAddress string, logger logging.Logger) *RTSPDiscovery {
	if multicastAddress == "" {
		multicastAddress = defaultMulticastAddress
	}
	return &RTSPDiscovery{
		multicastAddress: multicastAddress,
		logger:           logger,
	}
}

// generateDiscoveryMessage adds a message ID per xml message to adhere to standard formatting.
func (d *RTSPDiscovery) generateDiscoveryMessage() string {
	messageID := uuid.New().String()
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
	<SOAP-ENV:Envelope xmlns:SOAP-ENV="http://www.w3.org/2003/05/soap-envelope" 
                xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing" 
                xmlns:wsdd="http://schemas.xmlsoap.org/ws/2005/04/discovery">
		<SOAP-ENV:Header>
			<wsa:MessageID>uuid:%s</wsa:MessageID>
			<wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>
			<wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>
		</SOAP-ENV:Header>
		<SOAP-ENV:Body>
			<wsdd:Probe>
				<wsdd:Types>dn:NetworkVideoTransmitter</wsdd:Types>
			</wsdd:Probe>
		</SOAP-ENV:Body>
	</SOAP-ENV:Envelope>`, messageID)
}

// discoverRTSPAddresses performs a WS-Discovery and extracts http IP addresses from the XAddrs field.
func (d *RTSPDiscovery) discoverRTSPAddresses() ([]string, error) {
	var discoveredAddresses []string

	addr, err := net.ResolveUDPAddr("udp4", d.multicastAddress)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve UDP address: %w", err)
	}

	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create UDP socket: %w", err)
	}
	defer conn.Close()

	_, err = conn.WriteToUDP([]byte(d.generateDiscoveryMessage()), addr)
	if err != nil {
		return nil, fmt.Errorf("failed to send discovery message: %w", err)
	}

	err = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("failed to set read deadline: %w", err)
	}

	buffer := make([]byte, 8192)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				d.logger.Debug("Timed out after waiting.")
				return discoveredAddresses, nil
			}
			return nil, fmt.Errorf("error reading from UDP: %w", err)
		}

		response := buffer[:n]
		xaddrs, err := d.extractXAddrsFromProbeMatch(response)
		if err != nil {
			d.logger.Warnf("Failed to parse response: %w\n", err)
			continue
		}

		discoveredAddresses = append(discoveredAddresses, xaddrs...)
	}
}

// extractXAddrsFromProbeMatch extracts XAddrs from the WS-Discovery ProbeMatch response.
func (d *RTSPDiscovery) extractXAddrsFromProbeMatch(response []byte) ([]string, error) {
	type ProbeMatch struct {
		XMLName xml.Name `xml:"Envelope"`
		Body    struct {
			ProbeMatches struct {
				ProbeMatch []struct {
					XAddrs string `xml:"XAddrs"`
				} `xml:"ProbeMatch"`
			} `xml:"ProbeMatches"`
		} `xml:"Body"`
	}

	var probeMatch ProbeMatch
	err := xml.NewDecoder(bytes.NewReader(response)).Decode(&probeMatch)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling probe match: %w", err)
	}

	var xaddrs []string
	for _, match := range probeMatch.Body.ProbeMatches.ProbeMatch {
		for _, xaddr := range strings.Split(match.XAddrs, " ") {
			if strings.HasPrefix(xaddr, "http://") {
				xaddrs = append(xaddrs, xaddr)
			}
		}
	}

	return xaddrs, nil
}
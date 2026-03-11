package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	spectrumsrv "github.com/cooldogedev/spectrum/server"
	spectrumpacket "github.com/cooldogedev/spectrum/server/packet"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

// handleServer continuously reads packets from the server and forwards them to the client.
func handleServer(s *Session) {
loop:
	for {
		select {
		case <-s.ctx.Done():
			s.CloseWithError(context.Cause(s.ctx))
			break loop
		default:
		}

		server := s.Server()
		pk, err := server.ReadPacket()
		if err != nil {
			if server != s.Server() {
				continue loop
			}

			var disconnectErr *spectrumsrv.DisconnectError
			if errors.As(err, &disconnectErr) {
				s.CloseWithError(disconnectErr)
				break loop
			}

			server.CloseWithError(fmt.Errorf("failed to read packet from server: %w", err))
			if err := s.fallback(); err != nil {
				s.CloseWithError(fmt.Errorf("fallback failed: %w", err))
				break loop
			}
			continue loop
		}

		proc := s.Processor()

		switch pk := pk.(type) {
		case *spectrumpacket.Flush:
			ctx := NewContext()
			proc.ProcessFlush(ctx)
			if ctx.Cancelled() {
				continue loop
			}

			if err := s.client.Flush(); err != nil {
				s.CloseWithError(fmt.Errorf("failed to flush client's buffer: %w", err))
				logError(s, "failed to flush client's buffer", err)
				break loop
			}
		case *spectrumpacket.Latency:
			s.latency.Store(pk.Latency)
		case *spectrumpacket.Transfer:
			if err := s.Transfer(pk.Addr); err != nil {
				logError(s, "failed to transfer", err)
			}
		case *spectrumpacket.UpdateCache:
			s.SetCache(pk.Cache)
		case *packet.Disconnect:
			s.Disconnect(pk.Message)
			break loop
		case packet.Packet:
			if err := handleServerPacket(s, proc, pk); err != nil {
				s.CloseWithError(fmt.Errorf("failed to write packet to client: %w", err))
				logError(s, "failed to write packet to client", err)
				break loop
			}
		case []byte:
			ctx := NewContext()
			proc.ProcessServerEncoded(ctx, &pk)
			if ctx.Cancelled() {
				continue loop
			}

			if _, err := s.client.Write(pk); err != nil {
				s.CloseWithError(fmt.Errorf("failed to write packet to client: %w", err))
				logError(s, "failed to write packet to client", err)
				break loop
			}
		}
	}
}

// handleClient continuously reads packets from the client and forwards them to the server.
func handleClient(s *Session) {
	header := &packet.Header{}
	pool := s.client.Proto().Packets(true)
	var shieldID int32
	for _, item := range s.client.GameData().Items {
		if item.Name == "minecraft:shield" {
			shieldID = int32(item.RuntimeID)
			break
		}
	}

	clientDecode := make(map[uint32]struct{}, len(s.opts.ClientDecode))
	for _, id := range s.opts.ClientDecode {
		clientDecode[id] = struct{}{}
	}

loop:
	for {
		select {
		case <-s.ctx.Done():
			s.CloseWithError(context.Cause(s.ctx))
			break loop
		default:
		}

		payload, err := s.client.ReadBytes()
		if err != nil {
			s.CloseWithError(fmt.Errorf("failed to read packet from client: %w", err))
			logError(s, "failed to read packet from client", err)
			break loop
		}

		proc := s.Processor()
		if err := handleClientPacket(s, proc, header, pool, shieldID, clientDecode, payload); err != nil {
			s.Server().CloseWithError(fmt.Errorf("failed to write packet to server: %w", err))
		}
	}
}

// handleLatency periodically sends the client's current ping and timestamp to the server for latency reporting.
// The client's latency is derived from half of RakNet's round-trip time (RTT).
// To calculate the total latency, we multiply this value by 2.
func handleLatency(s *Session, interval int64) {
	ticker := time.NewTicker(time.Millisecond * time.Duration(interval))
	defer ticker.Stop()
loop:
	for {
		select {
		case <-s.ctx.Done():
			s.CloseWithError(context.Cause(s.ctx))
			break loop
		case <-ticker.C:
			latency := s.client.Latency().Milliseconds() * 2
			if err := s.Server().WritePacket(&spectrumpacket.Latency{Latency: latency, Timestamp: time.Now().UnixMilli()}); err != nil {
				logError(s, "failed to write latency packet", err)
			}
		}
	}
}

// handleServerPacket processes and forwards the provided packet from the server to the client.
func handleServerPacket(s *Session, proc Processor, pk packet.Packet) (err error) {
	ctx := NewContext()
	proc.ProcessServer(ctx, &pk)
	if ctx.Cancelled() {
		return
	}

	if s.opts.SyncProtocol {
		for _, latest := range s.client.Proto().ConvertToLatest(pk, s.client) {
			s.tracker.handlePacket(latest)
		}
	} else {
		s.tracker.handlePacket(pk)
	}
	return s.client.WritePacket(pk)
}

// handleClientPacket processes and forwards the provided packet from the client to the server.
func handleClientPacket(s *Session, proc Processor, header *packet.Header, pool packet.Pool, shieldID int32, clientDecode map[uint32]struct{}, payload []byte) (err error) {
	ctx := NewContext()
	srv := s.Server()

	if len(clientDecode) == 0 {
		proc.ProcessClientEncoded(ctx, &payload)
		if !ctx.Cancelled() {
			return srv.Write(payload)
		}
		return
	}

	packetID, n := decodeVaruint32(payload)
	if n == 0 {
		return errors.New("failed to decode header")
	}

	if _, needsDecode := clientDecode[packetID]; !needsDecode {
		proc.ProcessClientEncoded(ctx, &payload)
		if !ctx.Cancelled() {
			return srv.Write(payload)
		}
		return
	}

	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while decoding packet %v: %v", packetID, r)
		}
	}()

	factory, ok := pool[packetID]
	if !ok {
		return fmt.Errorf("unknown packet %d", packetID)
	}

	header.PacketID = packetID
	pk := factory()
	pk.Marshal(s.client.Proto().NewReader(bytes.NewBuffer(payload[n:]), shieldID, true))
	if s.opts.SyncProtocol {
		proc.ProcessClient(ctx, &pk)
		if ctx.Cancelled() {
			return
		}
		return srv.WritePacket(pk)
	}

	for _, latest := range s.client.Proto().ConvertToLatest(pk, s.client) {
		proc.ProcessClient(ctx, &latest)
		if ctx.Cancelled() {
			break
		}

		if err := srv.WritePacket(latest); err != nil {
			return err
		}
	}
	return
}

// decodeVaruint32 decodes a variable-length encoded uint32 directly from a byte slice.
// Returns the decoded value and the number of bytes consumed. Returns 0, 0 on error.
func decodeVaruint32(data []byte) (uint32, int) {
	var val uint32
	for i := 0; i < len(data) && i < 5; i++ {
		val |= uint32(data[i]&0x7F) << (uint(i) * 7)
		if data[i] < 0x80 {
			return val, i + 1
		}
	}
	return 0, 0
}

func logError(s *Session, msg string, err error) {
	select {
	case <-s.ctx.Done():
		return
	default:
	}

	if !errors.Is(err, context.Canceled) {
		s.logger.Error(msg, "err", err)
	}
}

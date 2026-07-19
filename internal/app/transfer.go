package app

import (
	"context"
	"fmt"

	"cablecheck/internal/peer"
	"cablecheck/internal/protocol"
	"cablecheck/internal/reporting"
)

// sendReportsCallback returns the coordinator's SendReports peer callback: it
// streams the already prepared allowlisted report files from dir to the
// worker. PrepareComplete has re-rendered them with peer capabilities before
// the session decides whether transfer runs. The mangle hook (nil in
// production) corrupts outbound chunk data at the sender boundary for the
// integration corruption scenario. A transfer failure is a warning by
// contract, so the returned error only feeds the session's warning path — it
// never changes classification or exit code.
func (a *App) sendReportsCallback(dir string) func(context.Context, *peer.ReportChannel) error {
	mangle := a.deps.hooks.mangleReportChunk
	return func(ctx context.Context, rt *peer.ReportChannel) error {
		return reporting.SendReports(ctx, dir, &reportSender{rt: rt, mangle: mangle})
	}
}

// receiveReportsCallback returns the worker's ReceiveReports peer callback: it
// consumes the streamed report files into dir, verifying each digest and
// keeping nothing partial on a failure.
func (a *App) receiveReportsCallback(dir string) func(context.Context, *peer.ReportChannel) error {
	return func(ctx context.Context, rt *peer.ReportChannel) error {
		return reporting.ReceiveReports(ctx, dir, &reportReceiver{rt: rt})
	}
}

// reportSender adapts a peer.ReportChannel into a reporting.SenderChannel by
// marshaling the reporting frame values into protocol report/report_chunk
// payloads and decoding routed report_ack frames.
type reportSender struct {
	rt *peer.ReportChannel
	// mangle rewrites a chunk's data before it is sent; nil in production.
	mangle func([]byte) []byte
}

// SendManifest sends the opening report manifest.
func (s *reportSender) SendManifest(_ context.Context, m reporting.Manifest) error {
	return s.rt.Send(protocol.TypeReport, "", protocol.ReportManifest{
		Files:     toProtocolFiles(m.Files),
		TotalSize: m.TotalSize,
	})
}

// SendChunk sends one report chunk, applying the corruption hook if present.
func (s *reportSender) SendChunk(_ context.Context, c reporting.ChunkFrame) error {
	data := c.Data
	if s.mangle != nil {
		data = s.mangle(append([]byte(nil), data...))
	}
	return s.rt.Send(protocol.TypeReportChunk, "", protocol.ReportChunk{
		Name:   c.Name,
		Seq:    c.Seq,
		Offset: c.Offset,
		Data:   data,
		Last:   c.Last,
	})
}

// RecvAck decodes the next routed report_ack frame.
func (s *reportSender) RecvAck(ctx context.Context) (reporting.AckFrame, error) {
	env, err := s.rt.Receive(ctx)
	if err != nil {
		return reporting.AckFrame{}, err
	}
	ack, err := protocol.DecodePayload[protocol.ReportAck](env)
	if err != nil {
		return reporting.AckFrame{}, fmt.Errorf("decode report_ack: %w", err)
	}
	return reporting.AckFrame{
		Name:     ack.Name,
		OK:       ack.OK,
		Declined: ack.Declined,
		Error:    ack.Error,
	}, nil
}

// reportReceiver adapts a peer.ReportChannel into a reporting.ReceiverChannel:
// it decodes routed report/report_chunk frames and marshals report_ack frames.
type reportReceiver struct {
	rt *peer.ReportChannel
}

// RecvFrame decodes the next routed report or report_chunk frame.
func (r *reportReceiver) RecvFrame(ctx context.Context) (reporting.Manifest, reporting.ChunkFrame, bool, error) {
	env, err := r.rt.Receive(ctx)
	if err != nil {
		return reporting.Manifest{}, reporting.ChunkFrame{}, false, err
	}
	switch env.Type {
	case protocol.TypeReport:
		m, derr := protocol.DecodePayload[protocol.ReportManifest](env)
		if derr != nil {
			return reporting.Manifest{}, reporting.ChunkFrame{}, false, fmt.Errorf("decode report manifest: %w", derr)
		}
		return reporting.Manifest{
			Files:     fromProtocolFiles(m.Files),
			TotalSize: m.TotalSize,
		}, reporting.ChunkFrame{}, true, nil
	case protocol.TypeReportChunk:
		c, derr := protocol.DecodePayload[protocol.ReportChunk](env)
		if derr != nil {
			return reporting.Manifest{}, reporting.ChunkFrame{}, false, fmt.Errorf("decode report chunk: %w", derr)
		}
		return reporting.Manifest{}, reporting.ChunkFrame{
			Name:   c.Name,
			Seq:    c.Seq,
			Offset: c.Offset,
			Data:   c.Data,
			Last:   c.Last,
		}, false, nil
	default:
		return reporting.Manifest{}, reporting.ChunkFrame{}, false,
			fmt.Errorf("unexpected transfer frame type %s", env.Type)
	}
}

// SendAck marshals and sends one report_ack frame.
func (r *reportReceiver) SendAck(_ context.Context, a reporting.AckFrame) error {
	return r.rt.Send(protocol.TypeReportAck, "", protocol.ReportAck{
		Name:     a.Name,
		OK:       a.OK,
		Declined: a.Declined,
		Error:    a.Error,
	})
}

// toProtocolFiles converts reporting manifest files into protocol shape.
func toProtocolFiles(files []reporting.TransferFile) []protocol.ReportFile {
	out := make([]protocol.ReportFile, len(files))
	for i, f := range files {
		out[i] = protocol.ReportFile{Name: f.Name, Size: f.Size, SHA256: f.SHA256}
	}
	return out
}

// fromProtocolFiles converts protocol manifest files into reporting shape.
func fromProtocolFiles(files []protocol.ReportFile) []reporting.TransferFile {
	out := make([]reporting.TransferFile, len(files))
	for i, f := range files {
		out[i] = reporting.TransferFile{Name: f.Name, Size: f.Size, SHA256: f.SHA256}
	}
	return out
}

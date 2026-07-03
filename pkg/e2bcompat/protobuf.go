// Copyright 2024 SandrPod
// Hand-rolled protobuf codec for the envd connect-rpc messages. E2B's SDK
// connect clients send binary protobuf (Content-Type application/proto or
// application/connect+proto), so the JSON handlers alone don't satisfy the real
// SDK. We encode/decode the specific envd messages with protowire (no codegen /
// generated structs needed). Field numbers come from spec/envd/*.proto.

package e2bcompat

import (
	"strings"

	"google.golang.org/protobuf/encoding/protowire"
)

// isProtoContentType reports whether a connect request carries binary protobuf.
func isProtoContentType(ct string) bool {
	return strings.Contains(ct, "proto")
}

// ─── decode: process ──────────────────────────────────────────────────────────

// decodeStartRequest extracts the ProcessConfig from a StartRequest
// (StartRequest.process = field 1).
func decodeStartRequest(b []byte) ProcessConfig {
	var cfg ProcessConfig
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				break
			}
			b = b[vn:]
			cfg = decodeProcessConfig(v)
			continue
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			break
		}
		b = b[fn:]
	}
	return cfg
}

// decodeProcessConfig decodes ProcessConfig{cmd=1, args=2, envs=3, cwd=4}.
func decodeProcessConfig(b []byte) ProcessConfig {
	var cfg ProcessConfig
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType: // cmd
			v, vn := protowire.ConsumeString(b)
			if vn < 0 {
				return cfg
			}
			b = b[vn:]
			cfg.Cmd = v
		case num == 2 && typ == protowire.BytesType: // args (repeated string)
			v, vn := protowire.ConsumeString(b)
			if vn < 0 {
				return cfg
			}
			b = b[vn:]
			cfg.Args = append(cfg.Args, v)
		case num == 4 && typ == protowire.BytesType: // cwd (optional)
			v, vn := protowire.ConsumeString(b)
			if vn < 0 {
				return cfg
			}
			b = b[vn:]
			s := v
			cfg.Cwd = &s
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return cfg
			}
			b = b[fn:]
		}
	}
	return cfg
}

// decodeStringField decodes a message with a single string at `field` (used for
// StatRequest.path=1, MakeDirRequest.path=1, RemoveRequest.path=1).
func decodeStringField(b []byte, field protowire.Number) string {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == field && typ == protowire.BytesType {
			v, vn := protowire.ConsumeString(b)
			if vn < 0 {
				break
			}
			return v
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			break
		}
		b = b[fn:]
	}
	return ""
}

// decodeListDirRequest decodes ListDirRequest{path=1, depth=2}.
func decodeListDirRequest(b []byte) (string, uint32) {
	var path string
	var depth uint32
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeString(b)
			if vn < 0 {
				return path, depth
			}
			b = b[vn:]
			path = v
		case num == 2 && typ == protowire.VarintType:
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return path, depth
			}
			b = b[vn:]
			depth = uint32(v)
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return path, depth
			}
			b = b[fn:]
		}
	}
	return path, depth
}

// decodeMoveRequest decodes MoveRequest{source=1, destination=2}.
func decodeMoveRequest(b []byte) (string, string) {
	return decodeStringField(b, 1), decodeStringField(b, 2)
}

// ─── encode: filesystem ───────────────────────────────────────────────────────

func fileTypeEnum(t FileType) uint64 {
	switch t {
	case FileTypeFile:
		return 1
	case FileTypeDirectory:
		return 2
	default:
		return 0
	}
}

// encodeEntryInfo encodes EntryInfo{name=1,type=2,path=3,size=4,mode=5,perm=6}.
func encodeEntryInfo(e EntryInfo) []byte {
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendString(b, e.Name)
	b = protowire.AppendTag(b, 2, protowire.VarintType)
	b = protowire.AppendVarint(b, fileTypeEnum(e.Type))
	b = protowire.AppendTag(b, 3, protowire.BytesType)
	b = protowire.AppendString(b, e.Path)
	b = protowire.AppendTag(b, 4, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(e.Size))
	if e.Mode != 0 {
		b = protowire.AppendTag(b, 5, protowire.VarintType)
		b = protowire.AppendVarint(b, uint64(e.Mode))
	}
	if e.Permissions != "" {
		b = protowire.AppendTag(b, 6, protowire.BytesType)
		b = protowire.AppendString(b, e.Permissions)
	}
	return b
}

// encodeMsgField wraps inner bytes as message field `num`.
func encodeMsgField(num protowire.Number, inner []byte) []byte {
	b := protowire.AppendTag(nil, num, protowire.BytesType)
	return protowire.AppendBytes(b, inner)
}

// encodeStatResponse encodes StatResponse{entry=1}.
func encodeStatResponse(e EntryInfo) []byte { return encodeMsgField(1, encodeEntryInfo(e)) }

// encodeEntryResponse is the {entry=1} shape shared by MakeDir/Move responses.
func encodeEntryResponse(e EntryInfo) []byte { return encodeMsgField(1, encodeEntryInfo(e)) }

// encodeListDirResponse encodes ListDirResponse{entries=1 repeated}.
func encodeListDirResponse(entries []EntryInfo) []byte {
	var b []byte
	for _, e := range entries {
		b = append(b, encodeMsgField(1, encodeEntryInfo(e))...)
	}
	return b
}

// ─── encode: process events (StartResponse{event=1: ProcessEvent}) ────────────

func wrapStartResponse(event []byte) []byte { return encodeMsgField(1, event) }

// procStartEvent encodes ProcessEvent{start=1: StartEvent{pid=1}}.
func procStartEvent(pid uint32) []byte {
	start := protowire.AppendTag(nil, 1, protowire.VarintType)
	start = protowire.AppendVarint(start, uint64(pid))
	return wrapStartResponse(encodeMsgField(1, start))
}

// Process output channels for procDataEvent (DataEvent field numbers).
const (
	chStdout = 1
	chStderr = 2
	chPTY    = 3
)

// procDataEvent encodes ProcessEvent{data=2: DataEvent{stdout=1|stderr=2|pty=3}}.
func procDataEvent(data []byte, channel int) []byte {
	inner := protowire.AppendTag(nil, protowire.Number(channel), protowire.BytesType)
	inner = protowire.AppendBytes(inner, data)
	return wrapStartResponse(encodeMsgField(2, inner))
}

// procEndEvent encodes ProcessEvent{end=3: EndEvent{exit_code=1 sint32, exited=2, status=3}}.
func procEndEvent(exitCode int32, status string) []byte {
	end := protowire.AppendTag(nil, 1, protowire.VarintType)
	end = protowire.AppendVarint(end, protowire.EncodeZigZag(int64(exitCode)))
	end = protowire.AppendTag(end, 2, protowire.VarintType)
	end = protowire.AppendVarint(end, 1) // exited=true
	end = protowire.AppendTag(end, 3, protowire.BytesType)
	end = protowire.AppendString(end, status)
	return wrapStartResponse(encodeMsgField(3, end))
}

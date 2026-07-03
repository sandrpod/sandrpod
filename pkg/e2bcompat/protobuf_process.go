// Copyright 2024 SandrPod
// Binary-protobuf codecs for the E2B Process service messages that back the
// pid-addressed surface (Connect / SendInput / SendSignal / Update / List).
// The JSON path uses ordinary struct (un)marshaling; these mirror it for
// clients that negotiate application/connect+proto.

package e2bcompat

import "google.golang.org/protobuf/encoding/protowire"

// decodeStartRequestFull decodes StartRequest{process=1, pty=2, tag=3, stdin=4},
// returning the process config, an optional PTY size, and the stdin flag.
func decodeStartRequestFull(b []byte) (ProcessConfig, *PTYSize, bool) {
	var cfg ProcessConfig
	var pty *PTYSize
	var stdin bool
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType: // process
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return cfg, pty, stdin
			}
			b = b[vn:]
			cfg = decodeProcessConfig(v)
		case num == 2 && typ == protowire.BytesType: // pty
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return cfg, pty, stdin
			}
			b = b[vn:]
			rows, cols := decodePTYSize(v)
			pty = &PTYSize{Rows: rows, Cols: cols}
		case num == 4 && typ == protowire.VarintType: // stdin
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return cfg, pty, stdin
			}
			b = b[vn:]
			stdin = v != 0
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return cfg, pty, stdin
			}
			b = b[fn:]
		}
	}
	return cfg, pty, stdin
}

// decodeSelectorPID pulls the pid out of a ProcessSelector{pid=1, tag=2}.
func decodeSelectorPID(b []byte) uint32 {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == 1 && typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				break
			}
			return uint32(v)
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			break
		}
		b = b[fn:]
	}
	return 0
}

// decodeConnectRequest decodes ConnectRequest{process=1: ProcessSelector}.
func decodeConnectRequest(b []byte) uint32 { return decodeSubMessagePID(b, 1) }

// decodeSignalRequest decodes SendSignalRequest{process=1, signal=2 (enum)}.
func decodeSignalRequest(b []byte) (pid uint32, signal int32) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			pid = decodeSelectorPID(v)
		case num == 2 && typ == protowire.VarintType:
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			signal = int32(v)
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return
			}
			b = b[fn:]
		}
	}
	return
}

// decodeInputRequest decodes SendInputRequest{process=1, input=2:
// ProcessInput{stdin=1, pty=2}}.
func decodeInputRequest(b []byte) (pid uint32, data []byte, isPTY bool) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			pid = decodeSelectorPID(v)
		case num == 2 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			data, isPTY = decodeProcessInput(v)
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return
			}
			b = b[fn:]
		}
	}
	return
}

// decodeProcessInput decodes ProcessInput{stdin=1, pty=2}.
func decodeProcessInput(b []byte) (data []byte, isPTY bool) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if typ == protowire.BytesType && (num == 1 || num == 2) {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			data = append([]byte(nil), v...)
			isPTY = num == 2
			continue
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			return
		}
		b = b[fn:]
	}
	return
}

// decodeUpdateRequest decodes UpdateRequest{process=1, pty=2: PTY{size=1:
// Size{rows=1, cols=2}}}.
func decodeUpdateRequest(b []byte) (pid uint32, rows, cols uint32) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		switch {
		case num == 1 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			pid = decodeSelectorPID(v)
		case num == 2 && typ == protowire.BytesType:
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			rows, cols = decodePTYSize(v)
		default:
			fn := protowire.ConsumeFieldValue(num, typ, b)
			if fn < 0 {
				return
			}
			b = b[fn:]
		}
	}
	return
}

// decodePTYSize decodes PTY{size=1: Size{rows=1, cols=2}}.
func decodePTYSize(b []byte) (rows, cols uint32) {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == 1 && typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				return
			}
			b = b[vn:]
			// Size{rows=1, cols=2}
			for len(v) > 0 {
				sn, st, k := protowire.ConsumeTag(v)
				if k < 0 {
					break
				}
				v = v[k:]
				if st == protowire.VarintType {
					val, vk := protowire.ConsumeVarint(v)
					if vk < 0 {
						break
					}
					v = v[vk:]
					if sn == 1 {
						rows = uint32(val)
					} else if sn == 2 {
						cols = uint32(val)
					}
					continue
				}
				fk := protowire.ConsumeFieldValue(sn, st, v)
				if fk < 0 {
					break
				}
				v = v[fk:]
			}
			return
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			return
		}
		b = b[fn:]
	}
	return
}

// decodeSubMessagePID decodes a message whose field `field` is a ProcessSelector
// and returns its pid.
func decodeSubMessagePID(b []byte, field protowire.Number) uint32 {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == field && typ == protowire.BytesType {
			v, vn := protowire.ConsumeBytes(b)
			if vn < 0 {
				break
			}
			return decodeSelectorPID(v)
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			break
		}
		b = b[fn:]
	}
	return 0
}

// decodeBoolField decodes a bool at `field` in a message (varint != 0).
func decodeBoolField(b []byte, field protowire.Number) bool {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			break
		}
		b = b[n:]
		if num == field && typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(b)
			if vn < 0 {
				break
			}
			return v != 0
		}
		fn := protowire.ConsumeFieldValue(num, typ, b)
		if fn < 0 {
			break
		}
		b = b[fn:]
	}
	return false
}

// encodeCreateWatcherResponse encodes CreateWatcherResponse{watcher_id=1}.
func encodeCreateWatcherResponse(id string) []byte {
	out := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendString(out, id)
}

// watchEventTypeEnum maps an E2B EventType name to its enum number.
func watchEventTypeEnum(name string) uint64 {
	switch name {
	case "EVENT_TYPE_CREATE":
		return 1
	case "EVENT_TYPE_WRITE":
		return 2
	case "EVENT_TYPE_REMOVE":
		return 3
	case "EVENT_TYPE_RENAME":
		return 4
	case "EVENT_TYPE_CHMOD":
		return 5
	}
	return 0
}

// encodeWatcherEventsResponse encodes GetWatcherEventsResponse{events=1:
// FilesystemEvent{name=1, type=2}}.
func encodeWatcherEventsResponse(events []WatchEvent) []byte {
	var out []byte
	for _, ev := range events {
		fe := protowire.AppendTag(nil, 1, protowire.BytesType)
		fe = protowire.AppendString(fe, ev.Name)
		fe = protowire.AppendTag(fe, 2, protowire.VarintType)
		fe = protowire.AppendVarint(fe, watchEventTypeEnum(ev.Type))
		out = append(out, encodeMsgField(1, fe)...)
	}
	return out
}

// encodeListResponse encodes ListResponse{processes=1: ProcessInfo{config=1:
// ProcessConfig{cmd=1,args=2,cwd=4}, pid=2, tag=3}}.
func encodeListResponse(procs []ProcInfo) []byte {
	var out []byte
	for _, p := range procs {
		cfg := protowire.AppendTag(nil, 1, protowire.BytesType)
		cfg = protowire.AppendString(cfg, p.Cmd)
		for _, a := range p.Args {
			cfg = protowire.AppendTag(cfg, 2, protowire.BytesType)
			cfg = protowire.AppendString(cfg, a)
		}
		if p.Cwd != "" {
			cfg = protowire.AppendTag(cfg, 4, protowire.BytesType)
			cfg = protowire.AppendString(cfg, p.Cwd)
		}
		info := encodeMsgField(1, cfg) // config
		info = protowire.AppendTag(info, 2, protowire.VarintType)
		info = protowire.AppendVarint(info, uint64(p.PID))
		if p.Tag != "" {
			info = protowire.AppendTag(info, 3, protowire.BytesType)
			info = protowire.AppendString(info, p.Tag)
		}
		out = append(out, encodeMsgField(1, info)...) // processes (repeated)
	}
	return out
}

package track

import (
	"io"
	"time"

	"go.uber.org/zap"
	"m7s.live/engine/v4/codec"
	. "m7s.live/engine/v4/common"
	"m7s.live/engine/v4/util"
)

var _ SpesificTrack = (*H265)(nil)

type H265 struct {
	Video
	VPS []byte `json:"-" yaml:"-"`
}

func NewH265(stream IStream, stuff ...any) (vt *H265) {
	vt = &H265{}
	vt.Video.CodecID = codec.CodecID_H265
	vt.SetStuff("h265", int(256), byte(96), uint32(90000), stream, vt, time.Millisecond*10)
	vt.SetStuff(stuff...)
	if vt.BytesPool == nil {
		vt.BytesPool = make(util.BytesPool, 17)
	}
	vt.ParamaterSets = make(ParamaterSets, 3)
	vt.nalulenSize = 4
	vt.dtsEst = NewDTSEstimator()
	return
}

func (vt *H265) WriteSliceBytes(slice []byte) {
	t := codec.ParseH265NALUType(slice[0])
	// fmt.Println("H265 NALU Type:", t)
	switch t {
	case codec.NAL_UNIT_VPS:
		vt.VPS = slice
		vt.ParamaterSets[0] = slice
	case codec.NAL_UNIT_SPS:
		vt.SPS = slice
		vt.ParamaterSets[1] = slice
		spsInfo, _ := codec.ParseHevcSPS(slice)
		if spsInfo.Width != vt.SPSInfo.Width || spsInfo.Height != vt.SPSInfo.Height {
			vt.Debug("SPS", zap.Any("SPSInfo", spsInfo))
		}
		vt.SPSInfo = spsInfo
	case codec.NAL_UNIT_PPS:
		vt.PPS = slice
		vt.ParamaterSets[2] = slice
		if vt.VPS != nil && vt.SPS != nil && vt.PPS != nil {
			extraData, err := codec.BuildH265SeqHeaderFromVpsSpsPps(vt.VPS, vt.SPS, vt.PPS)
			if err == nil {
				vt.WriteSequenceHead(extraData)
			} else {
				vt.Error("H265 BuildH265SeqHeaderFromVpsSpsPps", zap.Error(err))
				vt.Stream.Close()
			}
		}
	case
		codec.NAL_UNIT_CODED_SLICE_BLA,
		codec.NAL_UNIT_CODED_SLICE_BLANT,
		codec.NAL_UNIT_CODED_SLICE_BLA_N_LP,
		codec.NAL_UNIT_CODED_SLICE_IDR,
		codec.NAL_UNIT_CODED_SLICE_IDR_N_LP,
		codec.NAL_UNIT_CODED_SLICE_CRA:
		vt.Value.IFrame = true
		vt.AppendAuBytes(slice)
	case 0, 1, 2, 3, 4, 5, 6, 7, 8, 9:
		vt.Value.IFrame = false
		vt.AppendAuBytes(slice)
	case codec.NAL_UNIT_SEI:
		vt.AppendAuBytes(slice)
	default:
		vt.Warn("h265 slice type not supported", zap.Uint("type", uint(t)))
	}
}
func (vt *H265) writeSequenceHead(head []byte) (err error) {
	vt.WriteSequenceHead(head)
	if vt.VPS, vt.SPS, vt.PPS, err = codec.ParseVpsSpsPpsFromSeqHeaderWithoutMalloc(vt.SequenceHead); err == nil {
		vt.SPSInfo, _ = codec.ParseHevcSPS(vt.SequenceHead)
		vt.nalulenSize = (int(vt.SequenceHead[26]) & 0x03) + 1
	} else {
		vt.Error("H265 ParseVpsSpsPps Error")
		vt.Stream.Close()
	}
	return
}
func (vt *H265) WriteAVCC(ts uint32, frame *util.BLL) (err error) {
	if l := frame.ByteLength; l < 6 {
		vt.Error("AVCC data too short", zap.Int("len", l))
		return io.ErrShortWrite
	}
	b0 := frame.GetByte(0)
	if isExtHeader := (b0 >> 4) & 0b1000; isExtHeader != 0 {
		packetType := b0 & 0b1111
		switch packetType {
		case codec.PacketTypeSequenceStart:
			header := frame.ToBytes()
			header[0] = 0x1c
			header[1] = 0x00
			header[2] = 0x00
			header[3] = 0x00
			header[4] = 0x00
			err = vt.writeSequenceHead(header)
			frame.Recycle()
			return
		case codec.PacketTypeCodedFrames:
			frame.Next.Value[0] = b0 & 0b0111_1111 & 0xFC
			frame.Next.Value[1] = 0x01
			copy(frame.Next.Value[2:], frame.Next.Value[5:])
			frame.Next.Value = frame.Next.Value[:frame.Next.Value.Len()-3]
			frame.ByteLength -= 3
			return vt.Video.WriteAVCC(ts, frame)
		case codec.PacketTypeCodedFramesX:
			frame.Next.Value[0] = b0 & 0b0111_1111 & 0xFC
			frame.Next.Value[1] = 0x01
			frame.Next.Value[2] = 0
			frame.Next.Value[3] = 0
			frame.Next.Value[4] = 0
			return vt.Video.WriteAVCC(ts, frame)
		}
	} else {
		if frame.GetByte(1) == 0 {
			err = vt.writeSequenceHead(frame.ToBytes())
			frame.Recycle()
			return
		} else {
			return vt.Video.WriteAVCC(ts, frame)
		}
	}
	return
}

func (vt *H265) WriteRTPFrame(frame *RTPFrame) {
	rv := &vt.Value
	// TODO: DONL may need to be parsed if `sprop-max-don-diff` is greater than 0 on the RTP stream.
	var usingDonlField bool
	var buffer = util.Buffer(frame.Payload)
	switch frame.H265Type() {
	case codec.NAL_UNIT_RTP_AP:
		buffer.ReadUint16()
		if usingDonlField {
			buffer.ReadUint16()
		}
		for buffer.CanRead() {
			l := int(buffer.ReadUint16())
			if buffer.CanReadN(l) {
				vt.WriteSliceBytes(buffer.ReadN(l))
			} else {
				return
			}
			if usingDonlField {
				buffer.ReadByte()
			}
		}
	case codec.NAL_UNIT_RTP_FU:
		if !buffer.CanReadN(3) {
			return
		}
		first3 := buffer.ReadN(3)
		fuHeader := first3[2]
		if usingDonlField {
			buffer.ReadUint16()
		}
		if naluType := fuHeader & 0b00111111; util.Bit1(fuHeader, 0) {
			vt.WriteSliceByte(first3[0]&0b10000001|(naluType<<1), first3[1])
		}
		if rv.AUList.Pre != nil {
			rv.AUList.Pre.Value.Push(vt.BytesPool.GetShell(buffer))
		}
	default:
		vt.WriteSliceBytes(frame.Payload)
	}
	if frame.Marker {
		vt.generateTimestamp(frame.Timestamp)
		if !vt.dcChanged && rv.IFrame {
			vt.insertDCRtp()
		}
		vt.Flush()
	}
}

// RTP格式补完
func (vt *H265) CompleteRTP(value *AVFrame) {
	// H265打包： https://blog.csdn.net/fanyun_01/article/details/114234290
	var out [][][]byte
	if value.IFrame {
		out = append(out, [][]byte{vt.VPS}, [][]byte{vt.SPS}, [][]byte{vt.PPS})
	}
	vt.Value.AUList.Range(func(au *util.BLL) bool {
		if au.ByteLength < RTPMTU {
			out = append(out, au.ToBuffers())
		} else {
			var naluType codec.H265NALUType
			r := au.NewReader()
			b0, _ := r.ReadByte()
			b1, _ := r.ReadByte()
			naluType = naluType.Parse(b0)
			b0 = (byte(codec.NAL_UNIT_RTP_FU) << 1) | (b0 & 0b10000001)
			buf := [][]byte{{b0, b1, (1 << 7) | byte(naluType)}}
			buf = append(buf, r.ReadN(RTPMTU-3)...)
			out = append(out, buf)
			for bufs := r.ReadN(RTPMTU); len(bufs) > 0; bufs = r.ReadN(RTPMTU) {
				buf = append([][]byte{{b0, b1, byte(naluType)}}, bufs...)
				out = append(out, buf)
			}
			buf[0][2] |= 1 << 6 // set end bit
		}
		return true
	})
	vt.PacketizeRTP(out...)
}

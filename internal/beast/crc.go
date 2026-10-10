package beast

// Mode S CRC-24, using the same generator polynomial and syndrome
// computation as dump1090 (crc.c): the CRC of all bytes except the final
// three, XOR those three parity bytes. For Address/Parity messages the
// syndrome *is* the 24-bit ICAO address; for parity-only messages such as
// DF17 it is zero.

const crcGenerator = 0xfff409

var crcTable [256]uint32

func init() {
	for i := range crcTable {
		c := uint32(i) << 16
		for j := 0; j < 8; j++ {
			if c&0x800000 != 0 {
				c = (c << 1) ^ crcGenerator
			} else {
				c <<= 1
			}
		}
		crcTable[i] = c & 0x00ffffff
	}
}

// Syndrome returns the Mode S CRC syndrome of msg, which must include its
// final three parity bytes. It is the ICAO address for Address/Parity
// formats and zero for a message with intact parity.
func Syndrome(msg []byte) uint32 {
	if len(msg) < 3 {
		return 0
	}
	rem := uint32(0)
	for i := 0; i < len(msg)-3; i++ {
		rem = (rem << 8) ^ crcTable[msg[i]^byte(rem>>16)]
		rem &= 0xffffff
	}
	tail := msg[len(msg)-3:]
	return rem ^ uint32(tail[0])<<16 ^ uint32(tail[1])<<8 ^ uint32(tail[2])
}

// Package battleye speaks BattlEye's RCON protocol: the packet format, the
// login, a logged-in Session with a background reader and an optional
// keepalive, parsing what the server sends (server messages, and the
// `players` and `bans` replies), and kickall built from those pieces.
//
// It knows nothing about HTTP, the database or ReCON's own web session.
//
// Protocol cheat sheet. Transport: UDP. Every packet looks like:
//
//	'B' 'E' | CRC32 (4 bytes, little-endian) | 0xFF | type | payload...
//	\_header_/ \_ checksum of everything after it _/
//
// Packet types:
//
//	0x00 login    client -> server: payload = password
//	              server -> client: payload = 0x01 (accepted) or 0x00 (rejected)
//	0x01 command  client -> server: payload = seq byte + command text
//	              server -> client: payload = seq byte + response text
//	              Long responses are split: body = 0x00, <total parts>, <part index>, data
//	0x02 message  server -> client: unsolicited server message, payload = seq byte + text.
//	              The client MUST acknowledge it by sending a 0x02 packet with that seq.
//	              If the ACK is lost the server resends the same seq, so a
//	              client can see a message more than once.
//
// An empty command (seq byte only) works as a keepalive; the server drops
// clients that stay silent for roughly 45 seconds.
package battleye

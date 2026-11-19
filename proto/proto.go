// Package proto holds the wire types of pocketgo and its Go client. The
// service and every client import it; no client declares its own copy of a
// wire type (ADR-0006).
package proto

// Version is the protocol version. A client sends it with every request and
// the service refuses a version it does not speak.
const Version = 1

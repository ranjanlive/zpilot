package server

import "github.com/ranjanlive/zpilot/internal/channels"

// The server only needs the stateless verification helpers from channels.
var (
	waVerify    = channels.WhatsAppVerify
	waSignature = channels.WhatsAppSignatureOK
)

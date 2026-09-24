// Package client connects to the KeyForge daemon over its local WebSocket:
// it performs the hello handshake and reads the events the daemon pushes.
//
// Plugin authors normally don't use this package directly; the plugin
// package builds on it and handles launch info, dispatch and shutdown.
//
// A plugin connection may only call hello, so the client exposes no other
// methods. The WebSocket URL carries an auth token: the client never includes
// it in the errors it returns.
package client

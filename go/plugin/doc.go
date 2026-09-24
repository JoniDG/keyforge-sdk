// Package plugin runs a KeyForge plugin: the author writes one handler per
// action and the package takes care of the rest.
//
// Run reads the launch info the daemon passes in the KEYFORGE_PLUGIN_INFO
// environment variable, connects, performs the hello handshake, dispatches
// each action_invoked event to the handler registered for its action id and
// returns when the daemon closes the connection, which is how the daemon
// stops plugins.
//
// Handlers run one at a time, in the order the daemon fired them, so state
// kept per Invocation.Context (e.g. a toggle bound to two keys) needs no
// locking. A handler that needs to do slow work should start its own
// goroutine.
package plugin

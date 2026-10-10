"""Python SDK for KeyForge plugins and clients.

- `keyforge_sdk.plugin` runs a plugin: one handler per action, the SDK handles
  the launch info, the connection, dispatch and shutdown.
- `keyforge_sdk.client` is the connection underneath: connect, hello
  handshake, read events.
"""

# CLAUDE.md — keyforge-sdk

## Objetivo
Toolkit para devs que quieren escribir **plugins** o **clientes externos** del daemon KeyForge. Provee la **especificación del protocolo** en plano y **helpers convenientes** en cada lenguaje, sin obligar a usarlos (cualquier lenguaje con WebSocket + JSON puede hablar con KeyForge).

## Scope
- Documentación completa del protocolo WebSocket: handshake, mensajes, eventos del lifecycle de plugins.
- Helpers en Go (`go/`) — único lenguaje activo en esta fase.
- Slots reservados para Node (`node/`) y Python (`python/`) — placeholders por ahora.
- Manifiesto de plugin (`manifest.json`) — formato y validador.

**Fuera de scope:**
- No implementa el server (eso es `keyforge-core`).
- No define los schemas de mensajes (eso es `keyforge-protocol`).

## Layout
```
docs/                  → spec del protocolo en markdown (handshake, mensajes, lifecycle)
examples/              → plugins de ejemplo (echo, hello-world)
go/                    → SDK Go (ACTIVO)
  client/              → cliente WebSocket helper
  plugin/              → base de plugin con lifecycle hooks
node/                  → placeholder (TBD)
python/                → placeholder (TBD)
```

## Stack
- Go 1.24 (subdir `go/`).
- Tipos del protocolo: módulo publicado `github.com/JoniDG/keyforge-protocol/go` (tag `go/vX.Y.Z`; en `go get` va sin el prefijo: `@v0.10.0`). Nunca redefinir tipos del protocolo a mano.
- WebSocket: `github.com/coder/websocket` (misma lib que `keyforge-core`; API con `context`, y `websocket.Accept` sirve para el server fake de los tests). Ojo: cancelar el ctx de un `Read` cierra la conexión.
- Tests: `testify`, `mockery`.

## Comandos

Para el SDK Go:
```bash
cd go
make test
make lint
```

## Reglas duras

### Identidad del owner
- 👤 **Identidad del owner:**
  - LICENSE / copyright / contacto público: **Jonathan Daniel Gomez** / `jonathan.d.gomez98@gmail.com`
  - Commits / GitHub: **JoniDG** / `jonathan.d.gomez98+github@gmail.com`
- 🚨 **Antes de cada commit y push:** verificar `git config user.name` = `JoniDG` y `git config user.email` = `jonathan.d.gomez98+github@gmail.com`.

### API estable
- Cualquier cambio en la API pública del SDK Go es **breaking** y requiere bump de major version.
- Documentar cada función exportada con godoc.
- Cuando agreguemos `node/` o `python/`, **misma semántica** entre lenguajes para que un tutorial funcione cross-stack.

### Tipado
- Go: `interface{}` y `any` prohibidos.
- TS (futuro): `strict: true`, no `any`.

### Tests (Go)
- Coverage mínimo 95%.
- Tests del client deben usar un WS server fake (no levantar el daemon real).

### Plugins externos primero
- Diseñá la API pensando en un dev externo escribiendo su primer plugin en una tarde.
- Si una API requiere conocer internals de KeyForge, está mal diseñada.

## Para Claude — cómo ayudarme acá

- Para cualquier cambio en el helper de WebSocket, consultar Context7 sobre la lib elegida.
- Cada feature nueva del SDK requiere ejemplo en `examples/` que la demuestre.
- Antes de agregar `node/` o `python/`: revisar el SDK Go y replicar la misma forma. Consistencia > novedad por lenguaje.

## Referencias

- Contexto cross-repo: [`../CLAUDE.md`](../CLAUDE.md)
- Spec del protocolo: [`docs/`](./docs/) (en este repo)
- Schemas: `../keyforge-protocol/`

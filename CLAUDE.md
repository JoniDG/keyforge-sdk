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
docs/                  → vista del protocolo para autores de plugins (resume keyforge-protocol)
examples/go/<nombre>/  → plugins de ejemplo; cada uno es un módulo Go propio (go.mod con replace a ../../../go) + manifest.json
go/                    → SDK Go (ACTIVO)
  client/              → Dial (hello) + ReadEvent + Close
  plugin/              → Run: env var, conexión, dispatch y shutdown; el autor solo escribe Handlers
node/                  → placeholder (TBD)
python/                → placeholder (TBD)
```

## Stack
- Go 1.24 (subdir `go/`).
- Tipos del protocolo: módulo publicado `github.com/JoniDG/keyforge-protocol/go` (tag `go/vX.Y.Z`; en `go get` va sin el prefijo: `@v0.10.0`). Nunca redefinir tipos del protocolo a mano.
- WebSocket: `github.com/coder/websocket` (misma lib que `keyforge-core`; API con `context`, y `websocket.Accept` sirve para el server fake de los tests). Ojo: cancelar el ctx de un `Read` **bloqueado** corta la conexión sin close frame (el peer ve un cierre anormal); con un ctx ya cancelado de entrada, en cambio, no la cierra. Por eso `plugin` lee con `context.WithoutCancel` y, al cancelar, cierra con `conn.Close()` (close 1000).
- Tests: `testify`, `mockery`.
- Lint: `golangci-lint`, siempre la **última release** (decisión del owner): CI usa `version: latest` y `make lint` compara la instalada con la última de GitHub (sin red avisa y corre igual). Los examples no tienen Makefile ni `.golangci.yml` propios: comparten `go/.golangci.yml` (decidido 2026-09-29), local vía `make lint-examples` y en CI vía `--config`. Un salto de versión mayor (v3) puede romper CI hasta migrar `.golangci.yml` y, probablemente, subir también `golangci-lint-action`.

## Comandos

Para el SDK Go:
```bash
cd go
make test
make lint           # falla si la instalada no es la última release (la que usa CI)
make lint-examples  # lintea examples/go/* con go/.golangci.yml
make lint-install   # instala la última release de golangci-lint en $(go env GOPATH)/bin
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
- `t.Setenv` no se puede usar con `t.Parallel()`: el paquete `plugin` inyecta el lookup de la env var en un `run` interno, y solo un test no paralelo cubre `Run`.
- Los ejemplos de `examples/go/` llevan test (manifest válido contra `protocol.PluginManifest`, cada action con su handler) y corren en el job `examples-go` del CI.

### Runtime de plugins (`go/plugin`, decidido 2026-09-24)
- Dispatch **secuencial y en orden** (modelo de event loop de Stream Deck): el estado por `Invocation.Context` no necesita locks; un handler lento que necesite paralelismo lanza su goroutine.
- Un goroutine lee y encola (buffer de 128) para detectar el cierre aunque un handler esté corriendo y cancelarle el ctx.
- **Cola llena → se descarta** la invocación nueva con un `Warn` (decidido 2026-09-28). No se pausa la lectura: core escribe con `writeTimeout` de 2s y cierra la conexión si vence, y como no reinicia plugins, pausar terminaría matando el plugin. Core además ya descarta del lado del daemon (`ErrPluginQueueFull`), así que es coherente con fire-and-forget.
- **Panics en handlers se recuperan** en `dispatch` (decidido 2026-09-28): se loguean con stack y el plugin sigue, igual que `net/http` con cada request. Motivo: core no reinicia plugins caídos. Los panics en goroutines que lance el handler no se pueden recuperar y siguen matando el proceso.
- Cierre del daemon: `Run` devuelve `nil` si es normal (1000/1001) y error si es anormal. Los invocations en cola al cerrarse se descartan. Nunca reconecta.
- **Credenciales de un plugin** (decidido 2026-09-30, `examples/go/spotify`): el protocolo no da settings de plugin ni directorio de datos, así que un plugin que necesita login trae un subcomando (`login`) que se corre a mano una vez y guarda en `<UserConfigDir>/KeyForge/plugin-data/<id>/` (0600, escritura atómica). Fuera de `plugins/<id>/` porque instalar una versión nueva reemplaza esa carpeta. OAuth con PKCE y Client ID propio de cada usuario: ningún secreto viaja en el paquete.
- `Invocation` es alias del tipo generado `protocol.ActionInvokedSchemaJsonData`; los errores de handler y los frames inválidos se loguean (fire-and-forget: no hay a quién reportarlos).

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

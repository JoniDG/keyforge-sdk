# CLAUDE.md — keyforge-sdk

## Objetivo
Toolkit para devs que quieren escribir **plugins** o **clientes externos** del daemon KeyForge. Provee la **especificación del protocolo** en plano y **helpers convenientes** en cada lenguaje, sin obligar a usarlos (cualquier lenguaje con WebSocket + JSON puede hablar con KeyForge).

## Scope
- Documentación completa del protocolo WebSocket: handshake, mensajes, eventos del lifecycle de plugins.
- Helpers en Go (`go/`) y Node (`node/`, paquete npm `@jdg-keyforge/sdk`), con la misma semántica.
- Slot reservado para Python (`python/`) — placeholder por ahora.
- Manifiesto de plugin (`manifest.json`) — formato y validador.

**Fuera de scope:**
- No implementa el server (eso es `keyforge-core`).
- No define los schemas de mensajes (eso es `keyforge-protocol`).

## Layout
```
docs/                  → vista del protocolo para autores de plugins (resume keyforge-protocol) + tutorial Go (tutorial-go.md)
examples/go/<nombre>/  → plugins de ejemplo; cada uno es un módulo Go propio (go.mod con replace a ../../../go) + manifest.json
examples/node/<nombre>/ → plugins de ejemplo en Node; cada uno es un paquete npm propio (dependencia file:../../../node) + manifest.json
go/                    → SDK Go (ACTIVO)
  client/              → Dial (hello) + ReadEvent + Close
  plugin/              → Run: env var, conexión, dispatch y shutdown; el autor solo escribe Handlers
node/                  → SDK Node (TS estricto → ESM en dist/; subpaths ./client y ./plugin, mismos paquetes que Go)
  src/guards.ts        → validación en runtime de los frames que consume el SDK (interno)
  src/testing.ts       → daemon falso con `ws` para los tests (fuera del build y del coverage)
python/                → placeholder (TBD)
```

## Stack
- Go 1.24 (subdir `go/`).
- Tipos del protocolo: módulo publicado `github.com/JoniDG/keyforge-protocol/go` (tag `go/vX.Y.Z`; en `go get` va sin el prefijo: `@v0.10.0`). Nunca redefinir tipos del protocolo a mano.
- WebSocket: `github.com/coder/websocket` (misma lib que `keyforge-core`; API con `context`, y `websocket.Accept` sirve para el server fake de los tests). Ojo: cancelar el ctx de un `Read` **bloqueado** corta la conexión sin close frame (el peer ve un cierre anormal); con un ctx ya cancelado de entrada, en cambio, no la cierra. Por eso `plugin` lee con `context.WithoutCancel` y, al cancelar, cierra con `conn.Close()` (close 1000).
- Tests: `testify`, `mockery`.
- **Node** (`node/`, decidido 2026-10-05): Node ≥22.4, TS estricto, sin dependencias en runtime.
  - WebSocket **nativo** de Node (global `WebSocket`); `ws` solo como devDep para el daemon falso de los tests.
  - `@jdg-keyforge/protocol` solo trae tipos, así que los frames se validan con **guards a mano** (`src/guards.ts` + `isInvocation` en `plugin.ts`) que cubren lo que el SDK lee, no el schema completo.
  - **Límite de 32 KiB por frame** para igualar a Go (que lo hereda de `coder/websocket`): el WebSocket nativo no permite fijarlo, así que se chequea al recibir y se cierra con **4009** (espejo de 1009 en el rango privado: el `close()` nativo solo acepta 1000 y 3000–4999). No protege memoria (el frame ya llegó), pero el peer es el daemon local.
  - `Conn.close()` espera el close frame del daemon como máximo **5 s** (igual que `coder/websocket`); después deja de esperar, pero el WebSocket nativo no puede soltar el socket, así que un plugin que termina ahí llama a `process.exit` (ver `examples/node/counter/src/main.ts`). Abortar el `signal` de `run` aborta en el acto el `signal` del handler en curso y descarta la cola, como cancelar el ctx en Go.
  - Diferencias de forma con Go: `AbortSignal` en lugar de `context`; abortar un `readEvent` rechaza con el `reason` y **no** cierra la conexión; un handler que tira o rechaza equivale al panic recuperado de Go. Los miembros `@internal` (`runWithEnv`, `Conn.send`/`nextFrame`) quedan fuera del `.d.ts` por `stripInternal`.
  - Tests: Vitest con coverage v8 (umbral 95% en líneas, statements, funciones y branches). Lint: ESLint (typescript-eslint strict + stylistic) + Prettier, mismo config que `keyforge-desktop`; los examples de `examples/node/*` comparten el de `node/` vía `npm run lint:examples`.
  - Publish a npm: lo corre el owner, con tag `node/vX.Y.Z`.
- Lint: `golangci-lint`, siempre la **última release** (decisión del owner): CI usa `version: latest` y `make lint` compara la instalada con la última de GitHub (sin red avisa y corre igual). Los examples no tienen Makefile ni `.golangci.yml` propios: comparten `go/.golangci.yml` (decidido 2026-09-29), local vía `make lint-examples` y en CI vía `--config`. Un salto de versión mayor (v3) puede romper CI hasta migrar `.golangci.yml` y, probablemente, subir también `golangci-lint-action`.

## Comandos

Para el SDK Node:
```bash
cd node
npm ci
npm test               # vitest + coverage (gate 95%)
npm run lint           # ESLint + Prettier
npm run lint:examples  # lintea examples/node/* con el config de node/
npm run build          # dist/ (los examples lo necesitan: dependen de file:../../../node)
```

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
- `docs/tutorial-go.md` construye paso a paso `examples/go/counter`: si cambia uno, se actualiza el otro en el mismo PR (los fragmentos finales del tutorial, incluidos los tests del paso 7, son copia textual del ejemplo, que CI testea; los de pasos intermedios son versiones previas del mismo código).
- Antes de agregar `node/` o `python/`: revisar el SDK Go y replicar la misma forma. Consistencia > novedad por lenguaje.

## Referencias

- Contexto cross-repo: [`../CLAUDE.md`](../CLAUDE.md)
- Spec del protocolo: [`docs/`](./docs/) (en este repo)
- Schemas: `../keyforge-protocol/`

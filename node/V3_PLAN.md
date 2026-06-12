# Node.js v3 — Umsetzungsplan

Status: **in Umsetzung** (Branch `feat/node-v3`). Referenz-Implementierung: Go v3.0
(Merge-Commit `2b2dd17`). Ziel: dieselbe Twilio-kompatible Control-Plane für die
Node/TS-Implementierung, bei voller Beibehaltung der v2.1-Daten-Ebene
(Default-Verhalten: jeder Inbound-Call auto-answered und streamt zu `WS_TARGET_URL`).

### Fortschritt

| Milestone | Status |
|-----------|--------|
| A Fundament + Bridge-Erweiterung | ✅ fertig |
| B REST read-only (list/get) | ✅ fertig |
| C TwiML + Mid-Call Modify + `<Hangup>` | ✅ fertig (`<Dial>` warn-skipped bis E) |
| D Status-Callbacks (Kern + Emit-Verdrahtung) | ✅ fertig (E2E-Feuern via sipp in G) |
| E B2BUA `<Dial>` (Client-Dialog, Dual-Leg, Digest qop) | ⏳ offen — der XL-Teil |
| F Graceful Shutdown / Dual-Leg-BYE | ⏳ offen |
| G E2E-Parität (sipp) + Doku | ⏳ offen |

356 Unit-/Integrationstests grün, typecheck sauber (TS 6). `main` (Major-Bumps)
ist eingemergt.

---

## 1. Ziel & Parität-Latte

v3 legt über die bestehende SIP↔WebSocket-Media-Bridge eine **100 %
Twilio-kompatible** REST-Control-Plane plus B2BUA-`<Dial>`:

- REST API `/2010-04-01/Accounts/{AccountSid}/Calls{,/{CallSid}}.json`
  (list / read / modify-in-flight).
- TwiML-Verben `<Hangup/>`, `<Dial>` + `<Number>` mit voller Attribut-Parität.
- B2BUA-Forwarding mit Toll-Fraud-Guardrails + Caller-ID-Resolution-Chain.
- Privacy-Gate: WS-Stream wird **vor** dem Outbound-INVITE geschlossen.
- Status-Callbacks: `X-Twilio-Signature` HMAC-SHA1 **byte-kompatibel** mit
  `twilio-python`/`twilio-node`, monotone `SequenceNumber`, RFC-2822-Timestamp,
  3× Exp-Backoff-Retry, SSRF-Guard.

Parität-Latte (aus Projekt-Gedächtnis): Control-Plane-Features müssen Twilios
REST/TwiML **bit-für-bit** treffen, nicht nur "Twilio-ähnlich". Das heißt
konkret: dieselben Fixtures wie auf der Go-Seite wiederverwenden (HMAC-Fixtures,
sipp-Szenarien) — siehe §9.

---

## 2. Ausgangslage: Node heute vs. Go v3

### 2.1 Was Node heute hat (v2.1)

| Modul | Rolle |
|-------|-------|
| `src/sip/userAgent.ts` | REGISTER + OPTIONS-Keepalive + Inbound-Dispatch über **einen** UDP-Socket :5060. Liefert `SipHandle.sendRaw()`. |
| `src/bridge/callManager.ts` | Inbound INVITE→100→180→200→ACK, Session-Map (Key = SIP Call-ID), BYE/CANCEL, WS-Reconnect-Loop, Teardown. |
| `src/rtp/rtpHandler.ts` | Per-Call dgram-Socket, RTP-Header parse/build, SRTP, DTMF (RFC 4733). |
| `src/sip/sdp.ts` | SDP offer/answer (pure), Codec-Select. |
| `src/ws/wsClient.ts` | Twilio-Media-Streams-Protokoll, Drain-Pacing, mark/clear/dtmf. |
| `src/config/index.ts` | Zod-Schema, ~13 Env-Vars. |
| `src/observability/metrics.ts` | 5 unlabeled In-Memory-Counter/Gauges. |
| `src/index.ts` | Bootstrap, `http.createServer` mit `/health` + `/metrics`, Graceful Shutdown (5 s). |

### 2.2 Die zentrale Lücke

Go v3 stützt sich auf **sipgo**: Dialoge (Server *und* Client), Transaktionen,
`WaitAnswer` mit Auto-401/407-Digest (qop/cnonce/nc), `DialogClientCache` für
das Routing von Inbound-BYE auf Outbound-Dialoge. Die gesamte
Forwarder-Logik (`go/internal/sip/forwarder.go`, ~1400 Zeilen) delegiert die
schwierige SIP-Choreografie an sipgo.

Node hat **nichts davon**. `userAgent.ts` kennt nur: REGISTER-Response-Handling,
OPTIONS, und ein `firstLine.startsWith(...)`-Dispatch für Inbound-Requests.
Es gibt keine Client-Transaktion, keinen Dialog-Begriff, kein CSeq-Matching
für ausgehende Requests außer dem fest verdrahteten BYE im CallManager.

**Konsequenz:** Der mit Abstand größte Brocken ist `<Dial>` (Milestone E). Alles
davor (REST, TwiML-Parse, Hangup, Status-Callbacks) ist gut beherrschbar und
weitgehend reine Applikationslogik.

### 2.3 Abbildung Go-Pakete → Node-Module (Zielbild)

| Go-Paket | Node-Ziel | Aufwand |
|----------|-----------|---------|
| `internal/identity` | `src/identity/sid.ts` | klein |
| `internal/config` (v3-Vars) | `src/config/index.ts` (erweitern) | klein |
| `internal/observability/metrics.go` | `src/observability/metrics.ts` (ausbauen) | mittel |
| `internal/observability/logging.go` | `src/logger/index.ts` (Secret-Mask) | klein |
| `internal/api/*` (chi) | `src/api/*` (eigener Mini-Router) | mittel |
| `internal/twiml/*` | `src/twiml/*` | mittel |
| `internal/webhook/*` | `src/webhook/*` | mittel-hoch (Byte-Kompat) |
| `internal/sip/forwarder.go`+`digest.go`+`guardrails.go` | `src/sip/forwarder.ts` + `dialog.ts` + `digest.ts` + `guardrails.ts` | **hoch** |
| `internal/bridge/{session,manager,leg,state}.go` | `src/bridge/*` Refactor | **hoch** |

---

## 3. Zentrale Architektur-Entscheidung: SIP-Stack für die Outbound-/B2BUA-Seite

Das ist die Entscheidung, die alles andere bestimmt. Drei Optionen:

**Option A — Externe SIP-Library** (z. B. `sip`, `drachtio`).
- ✗ Verändert die Gesamt-Architektur; passt schlecht zum
  Shared-Socket-/Pinhole-Modell (der Outbound-INVITE muss aus demselben
  5060-Source-Port kommen wie REGISTER, sonst verwirft sipgate ihn).
- ✗ Schwergewichtig, neue Dependency, eigene Quirks; "drop-in" ist keine davon.
- ✗ Widerspricht dem schlanken, dependency-armen Stil der Node-Implementierung.

**Option B — Reines Hand-Rolling inline im Forwarder.**
- ✗ Die Client-Dialog-/Transaktions-Logik (Branch/CSeq-Matching, Retransmits,
  CANCEL-vor-200, ACK-on-2xx, Digest-Retry mit qop) ist genau die Stelle, an der
  sipgo Hunderte Zeilen kapselt. Inline im Forwarder wird das unwartbar.

**Option C — Eigenes, minimales Client-Dialog-Modul (Empfehlung).**
- Ein kleiner, zweckgebauter `src/sip/dialog.ts`, der auf dem **bestehenden**
  Shared-Socket aufsetzt (UAC-Seite): Outbound-INVITE senden, provisorische
  (100/18x) und finale (2xx/4xx/5xx/6xx) Antworten per Branch+CSeq matchen,
  ACK auf 2xx, CANCEL vor finaler Antwort, In-Dialog-BYE, und der
  401/407-Digest-Retry.
- `src/sip/digest.ts`: Digest mit `qop=auth` (cnonce, nc) — der bestehende
  REGISTER-Pfad in `userAgent.ts` macht nur das simple `qop`-lose MD5; das reicht
  für INVITE gegen den Trunk i. d. R. **nicht**.
- Der `userAgent.ts`-Socket bekommt einen Erweiterungspunkt: eingehende
  Responses/Requests, die nicht zur Registrierung gehören, werden an
  registrierte Dialog-Handler geroutet (analog zu sipgos `extraByeReaders`,
  `forwarder.go:420`).
- ✓ Bleibt dependency-frei, im Stil des Repos, hält das Pinhole-Modell.
- ✓ Klare Test-Naht (Dialog-Modul isoliert unit-testbar).

**Empfehlung: Option C.** Der Plan unten geht von C aus. Dies ist die eine
Entscheidung, bei der ich vor Milestone E ausdrücklich dein OK möchte — du hast
die SIP-Tiefe, um A/B/C final zu bewerten.

---

## 4. Ziel-Modul-Layout `node/src`

```
src/
  identity/sid.ts            # newCallSid(), deriveAccountSid(), Regex CA../AC..
  config/index.ts            # + v3 Env-Vars (zod)
  logger/index.ts            # + Secret-Mask-Writer (AUTH_TOKEN, SIP_PASSWORD)
  observability/metrics.ts   # + v3-Metriken (bounded-cardinality Enums)
  api/
    router.ts                # Mini-Router (Methode+Pfad-Pattern, AccountSid/CallSid-Param)
    server.ts                # Mount: SecurityHeaders → MaxBytes(64KB) → BasicAuth → Handler
    auth.ts                  # BasicAuth, timingSafeEqual
    errors.ts                # Twilio-Error-Shapes (code/message/more_info/status)
    json.ts                  # CallJSON, PageJSON, rfc2822()
    calls.ts                 # list/get/modify Handler + Parsing/XOR-Validierung
    midcallAdapter.ts        # Naht API↔bridge/twiml (PrepareDial/PerformDial/Terminate)
  twiml/
    parse.ts                 # <Response>-Parser (Verb-Reihenfolge, unknown→warn-skip)
    verbs.ts                 # Verb-Typen (Dial, Number, Hangup, unknownVerb)
    dispatch.ts              # Dispatch(chain, target); MidCallTarget/DialTarget Interfaces
    verbDial.ts / verbHangup.ts
  webhook/
    signer.ts                # Sign() HMAC-SHA1, byte-kompatibel
    ssrf.ts                  # validateCallbackURL(), isBlockedIP(), DialContext-Äquiv.
    subscription.ts          # subscriptionMatches(), resolveEventName()
    status.ts                # StatusClient: per-Call-Queue, Retry, enqueue/drainAndClose
  sip/
    userAgent.ts             # + Dialog-Routing-Hook, sendRaw bleibt
    dialog.ts                # UAC-Client-Dialog (INVITE/ACK/CANCEL/BYE, Response-Matching)
    digest.ts                # Digest qop=auth (cnonce/nc)
    forwarder.ts             # Dial(): Orchestrierung, Caller-ID-Chain, Timer, Result-Mapping
    guardrails.ts            # Allow-list (default-deny), per-session/per-minute Caps
    sdp.ts                   # + buildSdpOffer() (PCMU+telephone-event) für Outbound-Leg
    rtpHandler.ts            # bleibt; Outbound-Leg nutzt zweite Instanz
  bridge/
    state.ts                 # State-Enum + Transitions
    leg.ts                   # Leg-Abstraktion (legs[0]=caller, legs[1]=callee)
    session.ts               # aus callManager.ts herausgelöste Session (CloseStream, Terminate, Status)
    manager.ts               # CallManager: callSidIdx, recentlyTerminated (TTL), List/GetByCallSid, Port-Pool
    ws.ts                    # WS-Event-Schemas (connected/start/media/stop)
  index.ts                   # + REST-Server mounten, Forwarder/StatusClient verdrahten
```

> Hinweis: `callManager.ts` wird in `bridge/{session,manager,leg,state}.ts`
> aufgeteilt. Das ist ein nicht-trivialer Refactor (Milestone A.2) und sollte
> früh passieren, damit REST/TwiML/Dial auf der neuen Struktur aufsetzen.

---

## 5. Milestones (jeweils ein eigenständiger, lieferbarer PR)

Reihenfolge nach Risiko/Abhängigkeit. Jeder Milestone schließt mit grünem
`typecheck` + `vitest` + (ab C) Integrationstests ab.

### Milestone A — Fundament + Bridge-Refactor
**A.1 Identity / Config / Observability**
- `identity/sid.ts`: `newCallSid()` = `"CA"`+16 random bytes hex;
  `deriveAccountSid(sipUser)` = `"AC"`+ erste 16 Byte von SHA-256(sipUser) hex
  (deterministisch!). Regex `^CA[0-9a-f]{32}$`, `^AC[0-9a-f]{32}$`.
- `config`: v3-Vars ergänzen (siehe §6).
- `metrics.ts`: v3-Metriken + Bucketing-Funktionen (§7). Prometheus-Text-Output
  erweitern; Label-Sets bounded halten.
- `logger`: Secret-Mask-Writer, der `AUTH_TOKEN` + `SIP_PASSWORD` aus jedem
  Log-Output redacted (Writer-Wrap, nicht Feld-Hook). Telefonnummern/URLs nicht
  auto-maskiert (Go-Verhalten gleichziehen).

**A.2 Bridge-Refactor (`callManager.ts` → `bridge/*`)**
- `state.ts`: State-Enum. Go nutzt 8 Werte; für Node brauchen wir realistisch:
  `Dispatching, Streaming, DialingOut, Forwarding, HungUp, Terminated`.
- `session.ts`: Session-Objekt mit `callSid`/`accountSid`/`streamSid`/`from`/`to`/
  `startTime`, `state`, `legs[]`, `answeredAt`, `sipFinalCode`, `seqCounter`,
  `statusCallback`-Config; Methoden `closeStream(reason)`, `terminate(reason)`,
  `isActive()`, `status()`, `nextSequenceNumber()`.
  - `closeStream` = Privacy-Gate-Primitive: WS sauber schließen, aber RTP-Pfad
    am Leben lassen (für Dual-Leg). In Node ist die "wsCtx vs sessionCtx"-Trennung
    durch Flags + explizites Aufräumen der WS-Listener nachzubauen.
- `leg.ts`: `Leg` kapselt einen RTP-Endpunkt (+ optional Dialog). `legs[0]` =
  Caller (existierender RtpHandler + Inbound-Dialog-Daten), `legs[1]` = Callee.
- `manager.ts`: zusätzlich zur Call-ID-Map ein `callSidIdx` (CallSid→Session) und
  `recentlyTerminated` (CallSid→Snapshot, TTL 5 min, Sweep alle 30 s).
  `list()` (active+terminated, StartTime-desc), `getByCallSid()`.
  RTP-Port-Pool als explizites `acquirePort()`/`releasePort()` (statt
  Modul-Counter in `rtpHandler.ts`) — nötig für die Dual-Leg-Port-Verwaltung.
- **Achtung Test-Falle (Projekt-Gedächtnis):** Channel-/Unit-Only-Tests dürfen
  die Goroutine/Socket-Verdrahtung nicht vortäuschen. Jeder Refactor-Schritt
  braucht einen Test, der die *tatsächliche* Produktions-Verdrahtung trifft,
  nicht nur die isolierte Klasse.

### Milestone B — REST Control-Plane (read-only)
- `api/router.ts`: Mini-Router. Pfad-Pattern
  `/2010-04-01/Accounts/:AccountSid/Calls.json` (GET) und
  `/Calls/:CallSid.json` (GET/POST). Go nutzt chi; wir bauen einen ~50-Zeilen-
  Matcher (das Routen-Set ist winzig). Alternativ Mini-Dep — siehe Offene
  Entscheidungen §10.
- `api/security.ts`-Middleware-Kette in **genau dieser Reihenfolge**
  (lasttragend, durch Go-Tests gepinnt):
  `SecurityHeaders → MaxBytesReader(64KB) → BasicAuth → metrics → Handler`.
  - SecurityHeaders: `Strict-Transport-Security: max-age=63072000; includeSubDomains`,
    `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`,
    `X-Content-Type-Options: nosniff` — **auch auf 401/413**.
  - MaxBytes: zweistufig — Content-Length-Vorabprüfung (`> 65536` → 413) **und**
    Body-Stream-Cap. 413-Code `21617`.
- `api/auth.ts`: BasicAuth über `crypto.timingSafeEqual` (Username==AccountSid,
  Passwort==`AUTH_TOKEN`), zusätzlich URL-`{AccountSid}` constant-time gegen
  konfigurierte SID (Mismatch → 401, **nicht** 404, gegen Enumeration).
  Fehlende Creds → 401 + `WWW-Authenticate: Basic realm="Twilio API"` + Error 20003.
- `api/errors.ts`: Error-Shape `{code, message, more_info, status}`,
  `more_info = https://www.twilio.com/docs/errors/{code}`. Konstruktoren für
  20003/20404/21218/21220/12100/11200/21617/20429.
- `api/json.ts`:
  - `CallJSON` exakt (snake_case): `sid, account_sid, from, to, status,
    start_time, end_time, duration, direction, answered_by, parent_call_sid,
    api_version("2010-04-01"), uri, subresource_uris`. Nullable Felder als
    explizites `null` serialisieren (nicht weglassen) — SDKs verlassen sich drauf.
  - `PageJSON`: `page, page_size, start, end, uri, next_page_uri,
    previous_page_uri, first_page_uri, calls[]`. Leeres `calls` = `[]`, nie `null`.
  - `rfc2822(date)` = `"Mon, 27 Apr 2026 10:00:00 +0000"` (immer UTC). Zero-Time → `null`.
- `calls.ts`: `listCallsHandler` (Query `Page`/`PageSize`, default 0/50,
  PageSize clamp [1,1000]), `getCallHandler` (CallSid-Regex; malformed → **404+20404**,
  nicht 400). Status-Mapping aus Session-State.
- Status-Vokabular: `queued, ringing, in-progress, completed, busy, failed,
  no-answer, canceled`.

### Milestone C — TwiML + Mid-Call Modify
- `twiml/parse.ts`: `<Response>`-Parser. **Kein** Struct-Unmarshal — Verben in
  Dokumentreihenfolge sammeln. Unbekannte Verben **behalten** als
  `unknownVerb{name}` (nicht stillschweigend verwerfen) → Dispatcher loggt
  warn-and-skip. Root ≠ `<Response>` oder leer → Error 12100.
  - XML-Parser: Node hat kein eingebautes XML. Optionen in §10 — Empfehlung:
    kleiner Streaming-Parser oder `fast-xml-parser` (Mini-Dep) mit
    Reihenfolge-Erhalt. Twilio-Attribut-Casing ist **case-sensitive** (`callerId`,
    `timeout`, `hangupOnStar`, `statusCallbackEvent` …).
- `verbs.ts`: `Dial` (callerId, timeout*, timeLimit*, hangupOnStar, action,
  method, answerOnBridge*, statusCallback{,Method,Event}, NumberText, Number),
  `Number` (Text + per-Leg statusCallback-Overrides), `Hangup`, `unknownVerb`.
  - Defaults: `timeout=30`, `timeLimit=14400`, `method/statusCallbackMethod=POST`.
    Nicht-numerisches timeout/timeLimit → still ignorieren (Default greift).
  - `resolveDialTarget()`: `<Number>` schlägt bare-text; beides gesetzt → Number gewinnt.
  - `statusCallbackEvent`: space- **oder** komma-getrennt; Enum-Validierung
    (initiated/ringing/answered/in-progress/completed/busy/failed/no-answer/canceled);
    unbekannt → Parse-Error 12100.
- `dispatch.ts`: Interfaces `MidCallTarget {terminate(reason), log()}` und
  `DialTarget extends MidCallTarget {prepareDial(opts), performDial(...)}`.
  Dispatch läuft Verben sequentiell; `<Hangup>`/`<Dial>` sind **terminal**
  (return danach). `<Dial>` auf Target ohne DialTarget → warn-and-skip.
- `verbHangup.ts`: `terminate("hangup")`.
- `api/calls.ts` `modifyCallHandler` + `midcallAdapter.ts`:
  - Form-Parsing (`application/x-www-form-urlencoded`): `Twiml`/`Url`+`Method`/
    `Status=completed` sind **xor** (genau eines), plus optional `StatusCallback`
    allein (gültig, idempotente Subscription ohne Mutation).
  - `Twiml` > 4000 Zeichen → 12100. `Status` ≠ `completed` → 21218.
  - `Url`-Pfad: TwiML per HTTP fetchen (15 s Budget, Fallback-URL), Fetch-Fehler → 11200.
  - `Status=completed` ist **idempotent** auf bereits beendetem Call (→ 200, kein 21220);
    `Twiml`/`Url` auf beendetem Call → 21220.
  - Dispatch in Node: async (Handler antwortet sofort mit aktuellem Zustand);
    Test-Hook für synchronen Dispatch (Go: `SetAsyncDispatch(false)`).
  - `<Hangup>` Pfad nutzt nur `session.terminate()` — vollständig **ohne** Milestone E lieferbar.

### Milestone D — Status-Callbacks (Byte-Kompatibilität)
- `webhook/signer.ts`: `sign(authToken, url, params)` →
  1. Keys case-sensitiv ASCII-sortieren; 2. pro Key Werte dedupen + sortieren;
  3. Signing-String = `url` + Konkatenation `key+value` **ohne** Delimiter,
  Werte **roh** (kein URL-Encoding, `+` bleibt `+`); 4. HMAC-SHA1 mit
  `AUTH_TOKEN`, **Standard**-Base64 (nicht URL-safe). URL **verbatim** signieren
  (nicht normalisiert).
  - **Validierung gegen die geteilten Go-Fixtures** `python_fixtures.json` /
    `node_fixtures.json` (siehe `go/internal/webhook/testdata/`) — diese Tabelle
    1:1 als Vitest-Fixtures ziehen. Das ist die Akzeptanz für "byte-kompatibel".
- `webhook/ssrf.ts`: `validateCallbackURL()` (Pre-Flight, kein DNS): leer →
  Fehler; Schema ≠ https → Fehler; `localhost` → blockiert; IP-Literal via
  `isBlockedIP()`. Dial-Time: einmal auflösen, **alle** IPs prüfen, erste IP
  direkt verbinden (Anti-DNS-Rebinding). Blockiert: loopback, RFC-1918,
  link-local (inkl. 169.254.169.254), multicast, unspecified, CGNAT
  100.64/10, 0/8, 255.255.255.255/32; IPv4-mapped-IPv6 normalisieren.
  - `STATUS_CALLBACK_DEFAULT_URL` (Operator) → `trusted=true` Bypass des SSRF-Guards.
- `webhook/subscription.ts`: leer/nil = subscribe-to-all; explizit = match;
  Terminal-Fallback (busy/failed/no-answer/canceled → "completed" wenn nur
  "completed" abonniert); Lifecycle-Events fallen **nicht** zurück.
- `webhook/status.ts`: per-CallSid Queue (Tiefe 64) + serieller Worker;
  Backoff `[0, 1s, 2s, 4s]`, Per-Attempt-Timeout 4 s; Retry bei
  Netzwerkfehler/408/429/5xx; 2xx=Erfolg; SSRF nie retryen.
  `enqueue(callSid, evt)` / `drainAndClose(callSid, 30s)`.
  - Felder pro Event: `CallSid, AccountSid, From, To, Caller, Called, Direction,
    ApiVersion, CallStatus, Timestamp(RFC2822), SequenceNumber(0-indexed,
    monoton), CallbackSource("call-progress-events")`; terminal zusätzlich
    `CallDuration, Duration(ceil min), SipResponseCode` (nur wenn answered/erfasst).
  - Emit-Sites: Lifecycle in der SIP-/Forwarder-Schicht (initiated/ringing/
    answered/in-progress), Terminal in `session`-Teardown.
  - Default-StatusCallback (Operator) bei jedem Inbound-Call installieren, wenn
    konfiguriert.

### Milestone E — B2BUA `<Dial>` (der große Brocken)
> Setzt Option C aus §3 voraus.

**E.1 SIP-Client-Dialog (`sip/dialog.ts` + `sip/digest.ts`)**
- `userAgent.ts`: Routing-Hook ergänzen — Inbound-Responses/Requests, deren
  Call-ID/Branch zu einem registrierten Outbound-Dialog gehört, an dessen
  Handler zustellen (analog `extraByeReaders`). Shared-Socket bleibt :5060
  (Pinhole-Invariante).
- `dialog.ts` (UAC): Outbound-INVITE bauen+senden, 100/18x/2xx/4xx-6xx per
  `Via;branch` + CSeq matchen, ACK auf 2xx (eigene Transaktion), CANCEL vor
  finaler Antwort, In-Dialog-BYE, INVITE-Retransmit (T1..T2), Final-Retransmit-
  Absorption. `onResponse`-Callback für 18x→ringing-Event.
- `digest.ts`: `qop=auth` mit `cnonce`/`nc`, `MD5(method:uri)` für `ha2`,
  WWW-/Proxy-Authenticate-Parsing; 401 **und** 407. (Der REGISTER-Pfad in
  `userAgent.ts` macht nur qop-loses Digest — hier nicht wiederverwendbar.)

**E.2 Dual-Leg-RTP-Relay (`bridge/leg.ts` + `sip/sdp.ts`)**
- `sdp.ts`: `buildSdpOffer()` — PCMU (PT 0) + telephone-event (PT 101) only,
  `RTP/AVP` (kein SRTP auf Outbound-Leg). `acceptSdpAnswer()` validiert erste
  Audio-PT == 0; sonst **Codec-Fail-Fast** (ACK + sofortiges BYE,
  Result `failed`/`codec_mismatch`).
- `leg.ts`: Callee-Leg bekommt eigenen `RtpHandler` (zweiter dgram-Socket, Port
  aus Pool). Relay: Caller-RTP→Callee-RTP und Callee-RTP→Caller-RTP. In Node:
  die `'audio'`-Events der beiden RtpHandler kreuzweise verdrahten (statt
  WS-Pfad). 20-ms-Pacing + PCMU-Silence (0xFF) bei Lücken.
- State `Streaming → DialingOut → Forwarding`.

**E.3 Forwarder-Orchestrierung (`sip/forwarder.ts` + `guardrails.ts`)**
- `guardrails.ts`: `DIAL_ALLOWED_PREFIXES` Allow-list (leer = **deny-all**),
  Target-Normalisierung (`00`→`+`, Whitespace/Scheme strippen), per-Session-Cap
  (`DIAL_MAX_PER_SESSION`, default 3), globaler Rolling-Minute-Cap
  (`DIAL_MAX_PER_MINUTE`, default 60, 60×1-s-Buckets). Check **vor** dem
  `initiated`-Event.
- `forwarder.ts` `dial(callerSid, target, opts, calleeLeg)`:
  1. Guardrails-Check; 2. `initiated` (CallStatus=queued); 3. Caller-ID-Chain:
  TwiML `callerId` → `DIAL_DEFAULT_CALLER_ID` → `SIP_USER` → Inbound-From (ANI);
  Display-CID separat + `P-Preferred-Identity`; Normalisierung via
  `DIAL_CALLER_ID_COUNTRY_CODE`; 4. SDP-Offer; 5. INVITE über `dialog.ts`
  (Digest-Retry); 6. Ring-Timeout (`opts.timeout` ∨ `DIAL_RING_TIMEOUT_S`,
  +200 ms Watchdog-Grace); 7. 18x→ringing; 8. 2xx→SDP-Answer prüfen
  (Codec-Fail-Fast)→ACK→`onAnswered` (Relay an + State CAS); 9. TimeLimit-Timer;
  10. `hangupOnStar` (DTMF `*`→BYE); 11. Dialog-Ende abwarten, Result mappen.
- Result-Status→Reason: answered/hangup-star→`completed`, busy→`busy`,
  no-answer→`no-answer`, canceled→`canceled`, sonst `failed`.

**E.4 Privacy-Gate (harte Invariante)**
- In `midcallAdapter.prepareDial()`: **zuerst** `session.closeStream("dial-forward")`
  (WS-Stream zu, Bot getrennt), **dann** Port acquiren + Callee-Leg bauen,
  **dann erst** `performDial()` → INVITE. Reihenfolge nicht verhandelbar:
  der Bot darf das weitergeleitete Gespräch nie hören. Test, der die Reihenfolge
  WS-stop-vor-INVITE explizit prüft (Go: `e2e_privacy_gate_test.go`).

**E.5 Action-Callback**
- Nach Dial-Ende `action`-URL POSTen (best-effort, kein Retry), signiert mit
  `X-Twilio-Signature`. Felder: `DialCallSid, DialCallStatus(mapped),
  DialCallDuration, Direction="outbound-dial", CallSid, AccountSid`.

### Milestone F — Graceful Shutdown + Operational Hardening
- Drain-Budget anheben (Go: 15 s) und **dual-leg BYE** im Teardown (beide Legs).
- Shutdown-Sequenz: neue INVITEs 503, beide Legs BYE, WS-Stop, dann UNREGISTER.
- `drainAndClose` für StatusClient pro Session beim Terminate (kein
  Worker-Leak).
- Observability-Artefakte (Grafana-Dashboard / Prometheus-Rules) sind sprach-
  neutral und liegen schon unter `deploy/observability/` — nur Metrik-Namen
  zwischen Go/Node abgleichen.

### Milestone G — E2E-Parität + Doku
- sipp-Harness (`tests/e2e/sipp/`) ist sprachneutral: die 8 Szenarien gegen die
  Node-Binary laufen lassen (Inbound-Streaming, REST-Dial answer/busy/cancel/
  no-answer/codec-mismatch, REST-Hangup-mid-stream, Status-Callback-Flapping).
- Node-Integrationstests analog zur Go-Suite: shutdown, privacy-gate,
  Handle/Socket-Leak, CANCEL/BYE-Race.
- README-Tabelle "v3 control plane (Go today; Node to follow)" auf
  "Go + Node" umstellen; CHANGELOG/Release-Notes für Node ergänzen.

---

## 6. Neue Config-Env-Vars (zod-Schema in `src/config/index.ts`)

| Var | Default | Validierung |
|-----|---------|-------------|
| `AUTH_TOKEN` | "" (leer erlaubt, warnen wenn <32) | REST-BasicAuth-PW + HMAC-Key |
| `PUBLIC_BASE_URL` | optional | für HMAC-URL-Rekonstruktion hinter Proxy |
| `SIP_OUTBOUND_TARGET_PORT` | 0 | 0 oder [1,65535] |
| `STATUS_CALLBACK_DEFAULT_URL` | "" (disabled) | http(s):// |
| `STATUS_CALLBACK_DEFAULT_METHOD` | POST | POST\|GET |
| `STATUS_CALLBACK_DEFAULT_EVENTS` | `initiated,ringing,answered,completed` | CSV gültiger Events |
| `DIAL_ALLOWED_PREFIXES` | "" (= **deny-all**) | CSV `^\+?[0-9]+$` |
| `DIAL_DEFAULT_CALLER_ID` | optional | E.164 |
| `DIAL_CALLER_ID_COUNTRY_CODE` | optional | z. B. `49` (ohne +) |
| `DIAL_RING_TIMEOUT_S` | 30 | [5,600] |
| `DIAL_MAX_PER_SESSION` | 3 | ≥1 |
| `DIAL_MAX_PER_MINUTE` | 60 | ≥1 |

> Go hat zusätzlich `SIP_LISTEN_ADDR`; Node verdrahtet :5060 aktuell fest. Für
> Parität optional `SIP_LISTEN_ADDR` einführen (klein), sonst dokumentieren.

---

## 7. Metriken (bounded-cardinality)

Neu in `metrics.ts` (Label-Werte fix, niemals AccountSid/CallSid/Telefon/URL als Label):
- `twiml_parse_errors_total{code}` — code ∈ {12100,21218,21220,13xxx}
- `api_requests_total{route,method,status}` — route ∈ {list_calls,get_call,
  modify_call,health,metrics,unknown}; status ∈ {2xx,4xx,5xx}
- `api_request_duration_seconds{route}`
- `twiml_modify_total{kind,outcome}` — kind ∈ {twiml,url,status_completed}
- `forward_attempts_total`, `forward_success_total`,
  `forward_failed_total{reason}` (reason ∈ {busy,no_answer,rejected,unreachable,
  codec_mismatch,toll_fraud,rate_limit,caller_id_rejected,auth_failed,trunk_5xx,
  timeout,error}), `forward_duration_seconds{outcome}`,
  `auth_challenge_kind_total{kind∈401,407}`
- `rtp_port_pool_in_use`, `rtp_port_pool_size`, `rtp_port_acquire_failures_total`
- `status_callback_attempts_total{event}`, `status_callback_failures_total{reason}`
  (reason ∈ {timeout,4xx,5xx,connect_error,exhausted_retries,ssrf_rejected,queue_full})

> Go erzwingt die Cardinality-Disziplin per `cmd/lint-metrics` (AST-Walker). Für
> Node ist ein vergleichbarer Lint nicht zwingend — Konvention + Review reichen
> zunächst; ein kleiner ESLint-Custom-Rule-Check ist optional (§10).

---

## 8. Querschnittliche Fidelity-Punkte (leicht zu übersehen)

- **RFC 2822 / RFC 1123Z** Timestamp `"Mon, 27 Apr 2026 10:00:00 +0000"`, immer UTC.
  JS `toUTCString()` liefert `"... GMT"` — **nicht** identisch; eigener Formatter nötig.
- **SequenceNumber** 0-indexed, monoton pro Call (Status-Callbacks).
- **Nullable JSON-Felder** als explizites `null`, leeres `calls` als `[]`.
- **Malformed CallSid** → 404+20404, nicht 400.
- **HMAC**: Standard-Base64, kein Delimiter, rohe Werte, URL verbatim.
- **Digest INVITE** braucht qop/cnonce/nc — REGISTER-Pfad reicht nicht.
- **Shared-Socket-Pinhole**: Outbound-INVITE muss aus :5060 (REGISTER-Port) raus.
- **Privacy-Gate-Reihenfolge** ist eine harte, getestete Invariante.

---

## 9. Test- & Verifikationsstrategie

- **Wiederverwendung der geteilten Fixtures** (Twilio-100%-Kompat-Latte):
  - HMAC: `go/internal/webhook/testdata/{python,node}_fixtures.json` → Vitest.
  - sipp: `tests/e2e/sipp/*.xml` + `run-sipp.sh` gegen Node-Binary.
- Unit (vitest): signer, ssrf, subscription, twiml-parse, dial-target-resolve,
  json/rfc2822, errors, auth, guardrails, digest.
- Integration: REST list/get/modify, mid-call hangup, status-callback-lifecycle,
  privacy-gate-ordering, dual-leg-BYE, CANCEL/BYE-Race, Handle-/Socket-Leak.
- **Test-Falle aus Projekt-Gedächtnis beachten:** "channel-only"-Unit-Tests
  dürfen die echte Socket-/Event-Verdrahtung nicht überspringen — pro Feature
  mindestens ein Test, der die Produktions-Verdrahtung durchläuft.

---

## 10. Offene Entscheidungen (für deine Review)

1. **SIP-Stack Outbound/B2BUA** — Empfehlung **Option C** (eigenes minimales
   Client-Dialog-Modul, dependency-frei). Brauche dein OK vor Milestone E.
2. **REST-Routing** — Eigener Mini-Router (dependency-frei, ~50 Z.) vs. Mini-Dep.
   Empfehlung: eigener Mini-Router (Routen-Set ist winzig).
3. **XML-Parser für TwiML** — eigener Streaming-Parser vs. `fast-xml-parser`
   (Mini-Dep, Reihenfolge-erhaltend). Empfehlung: `fast-xml-parser`, spart Risiko;
   falls "zero new deps" Priorität hat → eigener Parser.
4. **Metrics-Cardinality-Lint** — Go hat `cmd/lint-metrics`. Für Node optional;
   Empfehlung: zunächst Konvention + Review, ESLint-Rule später.
5. **Sequencing/PR-Schnitt** — Empfehlung: A→B→C→D→E→F→G als getrennte PRs (jeder
   für sich nützlich; C liefert schon REST+Hangup ohne den Dial-Brocken).
6. **`SIP_LISTEN_ADDR`** — für volle Config-Parität einführen oder bewusst weglassen?

---

## 11. Grobe Aufwandseinschätzung (Richtwert)

| Milestone | Relativer Aufwand |
|-----------|-------------------|
| A Fundament + Bridge-Refactor | M |
| B REST read-only | M |
| C TwiML + Mid-Call (ohne Dial) | M |
| D Status-Callbacks | M–L (Byte-Kompat-Feintuning) |
| E B2BUA `<Dial>` | **XL** (SIP-Client-Dialog + Dual-Leg + Digest qop) |
| F Shutdown/Hardening | S–M |
| G E2E + Doku | M |

Milestone E dominiert; A–D sind parallelisierbar bzw. unabhängig lieferbar.
```

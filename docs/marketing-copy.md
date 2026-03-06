# sipgate SIP Stream Bridge — Marketing Copy
> Grundlage für die Website. Sprache: Deutsch. Ton: aufgeregt, klar, für jeden verständlich.

---

## Hero / Hauptbotschaft

### Headline
**Weg von Twilio. Rein in Deutschland.**

### Subline
Die sipgate SIP Stream Bridge lässt dich echte deutsche Telefonnummern statt Twilio nutzen — ohne eine einzige Zeile Code zu ändern.

### CTA
→ **Jetzt kostenlos loslegen** | → **Zur Dokumentation**

---

## Was ist das? (Für alle, die noch nie davon gehört haben)

Stell dir vor, du hast eine App gebaut, die telefonieren kann.
Die App spricht eine bestimmte Sprache — die Sprache von Twilio.

Das Problem: Twilio ist eine amerikanische Firma. Deine Telefongespräche gehen durch Server in den USA. Das macht Anwälte nervös. Und Datenschutzbeauftragte noch nervöser.

**Die sipgate SIP Stream Bridge ist die Lösung.**

Sie übersetzt die Twilio-Sprache deiner App in echte deutsche Telefonie — über sipgate, einem Anbieter aus Düsseldorf.

Deine App merkt nichts davon. Sie denkt immer noch, sie redet mit Twilio.
Aber in Wirklichkeit telefoniert sie über Deutschland.

> 🇺🇸 Deine App spricht Twilio → 🌉 Bridge übersetzt → 🇩🇪 sipgate-Anschluss in Deutschland

---

## Warum sipgate statt Twilio?

### 🇩🇪 Daten bleiben in Deutschland
Twilio ist eine US-amerikanische Firma. Wenn deine Telefonie über Twilio läuft, verlassen deine Gesprächsdaten Europa — und landen auf amerikanischen Servern, die amerikanischem Recht unterliegen.

Mit sipgate bleibt alles in Deutschland. sipgate betreibt seine Infrastruktur in Deutschland, unterliegt deutschem Recht und der DSGVO. Kein Umweg über den Atlantik. Kein Datenschutzproblem.

---

### 🔒 DSGVO — kein Kopfschmerz mehr
Die Datenschutz-Grundverordnung ist kein Spaß. Besonders nicht, wenn Telefongespräche personenbezogene Daten enthalten — und das tun sie fast immer.

Bei Twilio brauchst du:
- Einen Auftragsverarbeitungsvertrag mit einem US-Unternehmen
- Standardvertragsklauseln für den Datentransfer in die USA
- Einen Anwalt, der das alles prüft
- Schlaflose Nächte

Bei sipgate:
- Deutsches Unternehmen, deutsches Recht
- DSGVO-Compliance ohne Kopfrechnen
- Fertig

---

### 💶 Günstigere Telefonie, direkt in Deutschland
sipgate bietet echte deutsche Festnetz- und Mobilfunknummern zu fairen Preisen — direkt, ohne Umweg über einen amerikanischen Vermittler.

---

### 🤝 Support, der dich versteht
sipgate ist seit über 20 Jahren in Deutschland. Deutschsprachiger Support. Deutsche Mentalität. Wenn etwas nicht klappt, rufst du einfach an — auf Deutsch.

---

## Und was macht die Bridge genau?

Dein Backend spricht das **Twilio Media Streams Protokoll** — eine Sprache, die Twilio erfunden hat, damit Apps mit Telefonanrufen umgehen können.

Das Problem: sipgate spricht kein Twilio Media Streams. sipgate spricht **SIP** — den echten, offenen Telefoniestandard, den die ganze Welt benutzt.

Die Bridge sitzt zwischen beidem:

```
Dein Backend          sipgate SIP Stream Bridge       sipgate Telefonanlage
(spricht Twilio)  ←────────────────────────────────→  (spricht SIP / echtes Telefon)
```

Dein Backend muss **überhaupt nichts** ändern.
Du tauschst einfach Twilio gegen sipgate aus — und die Bridge macht den Rest.

---

## So einfach ist der Umstieg

### Schritt 1 — sipgate SIP-Zugangsdaten eintragen
```bash
SIP_USER=e12345p0
SIP_PASSWORD=meinPasswort
SIP_DOMAIN=sipconnect.sipgate.de
SIP_REGISTRAR=sipconnect.sipgate.de
WS_TARGET_URL=ws://mein-bestehendes-backend.example.com
SDP_CONTACT_IP=1.2.3.4
```

### Schritt 2 — Docker Image starten
```bash
docker run --env-file .env --network host \
  ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```

### Schritt 3 — Fertig
Dein bestehendes Backend läuft weiter — unverändert.
Nur die Telefonie kommt jetzt aus Deutschland.

**Drei Schritte. Fünf Minuten. Kein Code anfassen.**

---

## Docker Images — direkt und kostenlos

Die Images werden automatisch gebaut und sind jederzeit abrufbar — kostenlos, ohne Account, direkt aus der GitHub Container Registry.

### Go-Version (Empfohlen)
```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```
- Nur **~10 MB** groß
- Startet in Millisekunden
- Läuft stabil über Wochen ohne Neustart

### Node.js-Version (Referenz-Implementation)
```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-node:latest
```
- Selbe Features, selbe Konfiguration
- Ideal für alle, die den Code lesen und anpassen wollen

### Verfügbare Tags
| Tag | Bedeutung |
|-----|-----------|
| `latest` | Immer die neueste stabile Version |
| `main` | Aktuellster Stand aus dem Hauptbranch |
| `sha-abc1234` | Exakt ein bestimmter Commit |
| `v1.2.3` | Festgepinnte Version für Produktion |

---

## Twilio vs. sipgate — auf einen Blick

| | Twilio | sipgate + Bridge |
|--|--|--|
| 🌍 Serverstandort | USA 🇺🇸 | Deutschland 🇩🇪 |
| 🔒 DSGVO | Aufwändig, Standardvertragsklauseln nötig | Eingebaut, kein Extra-Aufwand |
| 📞 Rufnummern | International | Deutsche Festnetz & Mobilfunk |
| 💬 Support | Englisch, Ticketsystem | Deutsch, direkt |
| 💻 Code-Änderungen | — | **Keine** — dein Backend bleibt wie es ist |
| 🐳 Deployment | — | Ein Docker-Befehl |
| 💰 Kosten | Nutzungsabhängig | Bridge kostenlos |
| 🖥️ Hosting | Twilio-Infrastruktur | Selbst gehostet — ein Container, ~10 MB |

---

## Kostenlos. Selbst gehostet. Kein Problem.

Die Bridge kostet nichts. Dafür läuft sie bei dir, nicht bei uns.

Selbst hosten klingt nach Arbeit. Ist es aber nicht. Das Docker-Image ist gerade mal **~10 MB groß** — kleiner als die meisten App-Icons. Es läuft auf:

- dem kleinsten Cloud-Server (1 vCPU, 512 MB RAM reichen)
- direkt neben deinem bestehenden Backend

Kein separater Dienst, kein Vendor-Lock-in, keine Überraschungsrechnung am Monatsende. Du hast die volle Kontrolle — und das passt übrigens auch perfekt zur DSGVO: Deine Telefonie, deine Infrastruktur, deine Regeln.

---

## Was bleibt gleich?

Alles, was dein Backend schon kann — bleibt. Die Bridge unterstützt das vollständige Twilio Media Streams Protokoll:

- ✅ Eingehende und ausgehende Audiospuren
- ✅ DTMF-Töne (Tastatureingaben am Telefon)
- ✅ Mark / Clear (für präzise Audiokontrolle)
- ✅ Automatisches Reconnect wenn das Backend kurz weg ist
- ✅ Bis zu 100 parallele Gespräche
- ✅ Health Check unter `/health`
- ✅ Prometheus Metriken unter `/metrics`

---

## FAQ

**Muss ich meinen Code umschreiben?**
Nein. Die Bridge spricht exakt dieselbe Sprache wie Twilio Media Streams. Dein Backend merkt keinen Unterschied.

**Brauche ich einen sipgate Account?**
Ja — du brauchst einen sipgate SIP-Trunkanschluss. Den bekommst du unter [sipgatetrunking.de](https://www.sipgatetrunking.de).

**Ist das wirklich DSGVO-konform?**
sipgate ist ein deutsches Unternehmen mit deutschen Servern. Deine Telefondaten verlassen Deutschland nicht. Das ist ein erheblicher Vorteil gegenüber US-Anbietern.

**Was kostet die Bridge?**
Die Bridge ist komplett kostenlos. Du hostest sie selbst — aber keine Sorge: Das klingt komplizierter als es ist. Das Docker-Image ist nur ~10 MB groß und läuft problemlos auf dem kleinsten Cloud-Server oder direkt neben deinem bestehenden Backend. Ein einzelner `docker run`-Befehl reicht. Du zahlst nur für deinen sipgate-Anschluss.

**Wie viele Anrufe kann die Bridge gleichzeitig verarbeiten?**
Standardmäßig bis zu 100 parallele Gespräche — konfigurierbar nach oben.

**Läuft das auch ohne Linux?**
Für Produktion empfehlen wir Linux (`--network host`). Lokal auf macOS oder Windows funktioniert Docker Desktop ebenfalls.

---

## Los geht's

```bash
docker pull ghcr.io/sipgate/sipgate-sip-stream-bridge-go:latest
```

→ **[GitHub Repository](https://github.com/sipgate/sipgate-sip-stream-bridge)**
→ **[Dokumentation & Konfiguration](https://github.com/sipgate/sipgate-sip-stream-bridge#readme)**
→ **[sipgate SIP-Trunk buchen](https://www.sipgatetrunking.de)**

---

*sipgate SIP Stream Bridge — Made in Germany.*

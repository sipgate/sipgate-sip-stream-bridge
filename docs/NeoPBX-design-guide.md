# Neo PBX / sipgate Design Guide
> Living Styleguide — Stand: August 2024
> Erstellt von g31 (Strategie, Design, Storytelling)

---

## 1. Marke & Persönlichkeit

### Designvision
**"Smarte Techvibes + warmer Minimalismus"**

### Brand Personality — 3 Säulen

| Pilllar | Motto | Haltung |
|--------|-------|---------|
| **01 smart** | Beeindrucke mit smarten Lösungen | Pragmatische Problemlöser:innen — einfache Lösungen, flexibel, anpassungsfähig |
| **02 souverän** | Sei klar und verbindlich | Klarer Plan, Gelassenheit, Fokus, Struktur, erfahren, optimistisch |
| **03 supportive** | Unterstütze, wo du kannst | Zugänglich, offen, nahbar, zuhören, verstehen |

### Designprinzip
> So viele Regeln, wie nötig. So wenige, wie möglich.

---

## 2. Typografie

### Schriften

| Font | Schnitte | Charakter | Einsatz |
|------|----------|-----------|---------|
| **PX Grotesk** | Regular, Bold | Smart Tech — pixel-basiert, hybrid, stylisch & geeky | Primärschrift für alle Texte |
| **Tiempos Headline** | Light, Regular | Human Warmth — elegant, warm, zugänglich | Nur für große Headlines |

### Schriftmischung (Signature-Look)
Headlines erster Ordnung kombinieren **PX Grotesk Bold** + **Tiempos Headline Light**.

**Optische Angleichung:**
- Tiempos-Größe = PX-Grotesk-Größe × **0,96**
- Tiempos immer um **2% gesperrt** (letter-spacing)

### Typografische Hierarchie

#### Im Produkt (App / Dashboard)
| Ebene | Font | Stil |
|-------|------|------|
| H1 | PX Grotesk Bold + Tiempos Headline Light | Schriftmischung |
| H2 | PX Grotesk Bold | — |
| H3 / Buttons | PX Grotesk Bold | — |
| Body / Copy | PX Grotesk Regular | — |

#### Im Marketing (Website / Kampagnen)
| Ebene | Font | Stil |
|-------|------|------|
| H1 | PX Grotesk Bold + Tiempos Headline Light | Schriftmischung |
| H2 | Tiempos Headline Regular | — |
| H3 / Hervorhebungen | PX Grotesk Bold | — |
| Body / Copy | PX Grotesk Regular | — |

### Type Scale

Die Brand Library definiert 10 benannte Größenstufen. Alle drei Schriften (PX Grotesk Bold / Regular / Tiempos Headline Light) skalieren auf denselben Stufen.

| Token | Einsatz |
|-------|---------|
| `5XL` | Hero Headlines, Stage-Titel |
| `4XL` | Große Seitenüberschriften |
| `3XL` | Abschnitts-Headlines |
| `2XL` | Unter-Headlines, Feature-Titel |
| `XL` | Kleinere Headlines, Teaser |
| `L` | Lead-Texte, prominente Beschriftungen |
| `M` | Standard-Headlines in UI |
| `S` | Labels, kleine Buttons, UI-Text |
| `D` | Keyword / Detail-Hervorhebung + Lauftext |
| `XS` | Captions, Footnotes, Micro-Copy |

> Konkrete px/rem-Werte sind im Figma-Tokensystem definiert. Die Skala ist die verbindliche Referenz für Hierarchie-Entscheidungen.

### Zeilenabstand (ZAB)

| Kontext | Breite | ZAB |
|---------|--------|-----|
| Headlines | breit | 120% |
| Headlines | mittelbreit | 115% |
| Headlines | schmal | 110% |
| Copy | breit | 130% |
| Copy | mittelbreit | 125% |
| Copy | schmal | 120% |

### CSS Font Stack (Beispiel)
```css
--font-primary: 'PX Grotesk', sans-serif;
--font-headline: 'Tiempos Headline', serif;

/* H1 Schriftmischung */
.headline-mix span.grotesk { font-family: var(--font-primary); font-weight: 700; }
.headline-mix span.tiempos { font-family: var(--font-headline); font-weight: 300; font-size: 0.96em; letter-spacing: 0.02em; }
```

---

## 3. Farben

### Farbverhältnis (Color Ratio)

| Farbe | Hex | Anteil |
|-------|-----|--------|
| White | `#FFFFFF` | ~50% |
| Black | `#121212` | ~15% |
| Electric Lime | `#DEFF00` | ~15% |
| Mint dark | `#006660` | ~5% |
| Mint | `#D1E6D1` | ~5% |
| Cornflower | `#AABCFF` | ~3% |
| Clementine | `#FF7200` | ~3% |
| Lavender light | `#E2D8FF` | ~3% |

> Primär White als Hintergrund; Black nur in Sonderfällen.
> **Electric Lime** ist die prominenteste Brand Color — führend einsetzen.

### Meta Colors (übergreifend)

```css
--color-white:      #FFFFFF;
--color-black:      #121212;
--color-grey-light: #F7F7F7;
--color-grey:       #E7E6E6;
--color-grey-dark:  #818885;
```

### Brand Colors

Inkl. Pantone-Referenz, CMYK und WCAG AAA Kontrastverhältnis (auf Weiß):

| Name | Hex | Pantone | CMYK | WCAG AAA |
|------|-----|---------|------|----------|
| Electric Lime | `#DEFF00` | 809 C | 16, 0, 88, 0 | 16.42 |
| Mint | `#D1E6D1` | 9543 C | — | 14.22 |
| Dark Mint | `#006660` | 329 C | — | 6.84 |
| Cornflower | `#AABCFF` | 659 C | — | 10.12 |
| Clementine | `#FF7200` | 1505 C | — | 6.83 |
| Lavender | `#E2D8FF` | 9363 C | — | 13.82 |

```css
--color-electric-lime: #DEFF00;
--color-mint:          #D1E6D1;
--color-mint-dark:     #006660;
--color-cornflower:    #AABCFF;
--color-clementine:    #FF7200;
--color-lavender:      #E2D8FF;
```

### Interface Colors (UI-Palette, vollständig)

```css
/* Electric Lime */
--color-electric-lime-light: #F2FFB1;
--color-electric-lime:       #DEFF00;
--color-electric-lime-dark:  #202500;

/* Mint */
--color-mint-light:  #F1F7F7;
--color-mint:        #D1E6D1;
--color-mint-dark:   #006660;

/* Cornflower */
--color-cornflower-light: #D6DFFF;
--color-cornflower:       #AABCFF;
--color-cornflower-dark:  #315DFF;

/* Clementine */
--color-clementine-light: #FFDBBD;
--color-clementine:       #FF7200;
--color-clementine-dark:  #723401;

/* Lavender */
--color-lavender-light: #E2D8FF;
--color-lavender:       #C1ABFF;
--color-lavender-dark:  #342364;
```

### Soft Colors (neue Palette aus Brand Library)

Sehr helle, pastell-nahe Töne — für große Flächen, sanfte Hintergründe und weiche Verläufe.

| Name | Hex | Pantone |
|------|-----|---------|
| Soft Lime | `#EFFDC1` | 937 C |
| Soft Mint | `#EBF5EB` | 9540 C |
| Soft Cornflower | `#EDF2FF` | 9384 C |
| Soft Clementine | `#FFEADC` | 9241 C |
| Soft Lavender | `#F3F0FF` | 9340 C |

```css
--color-soft-lime:       #EFFDC1;
--color-soft-mint:       #EBF5EB;
--color-soft-cornflower: #EDF2FF;
--color-soft-clementine: #FFEADC;
--color-soft-lavender:   #F3F0FF;
```

### Alternate Colors (neue Palette aus Brand Library)

Gesättigte elektrische Töne für Akzente und Kontrastsituationen.

| Name | Hex | Pantone |
|------|-----|---------|
| Electric Cornflower | `#274FF0` | 285 C |
| Electric Lavender Dark | `#322372` | 2755 C |
| Electric Lavender | `#8642FE` | 2665 C |

```css
--color-electric-cornflower:    #274FF0;
--color-electric-lavender-dark: #322372;
--color-electric-lavender:      #8642FE;
```

### States Colors (funktional, nur UI)

```css
/* Success */
--color-green-light: #CCF2E6;
--color-green:       #00BD82;
--color-green-dark:  #006660;

/* Warning */
--color-orange-light: #FFF2CC;
--color-orange:       #FFBD00;
--color-orange-dark:  #866300;

/* Error */
--color-red-light: #FFDFDF;
--color-red:       #FF5E5E;
--color-red-dark:  #952314;
```

### Verläufe (Gradients)

Aus der Brand Library, mit präzisen Farbstops. Verlaufsrichtung je nach Layout frei wählbar.

**Monochrome** (Brand Color → transparent/weiß):
```css
--gradient-electric-lime: linear-gradient(to bottom, #DEFF00 0%, #EFFDC1 50%, #EBF5EB 75%, #EDF2FF 100%);
--gradient-clementine:    linear-gradient(to bottom, #FF7200 0%, #F3F0FF 100%);
--gradient-mint-dark:     linear-gradient(to bottom, #006660 0%, #EDF2FF 100%);
--gradient-cornflower:    linear-gradient(to bottom, #AABCFF 0%, #EDF2FF 100%);
--gradient-lavender:      linear-gradient(to bottom, #E2D8FF 0%, #F3F0FF 100%);
```

**Duochrome** (zwei Brand Colors):
```css
--gradient-cornflower-lavender:  linear-gradient(to bottom, #AABCFF 0%, #E2D8FF 100%);
--gradient-lavender-clementine:  linear-gradient(to bottom, #E2D8FF 0%, #FFDBBD 100%);
--gradient-cornflower-mint:      linear-gradient(to bottom, #AABCFF 0%, #D1E6D1 20%, #EBF5EB 100%);
```

**Soft** (sanfte Pastellverläufe):
```css
--gradient-soft-lime-cornflower: linear-gradient(to bottom, #EFFDC1 0%, #EDF2FF 100%);
--gradient-soft-lime-lavender:   linear-gradient(to bottom, #EFFDC1 0%, #F3F0FF 30%, #E2D8FF 100%);
```

> Verläufe sparsam und punktuell einsetzen — für Dynamik und digitalen Charakter.

---

## 4. Logo

### Bild-Marke (Icon)
- Zwei Kreise im **36°-Winkel** angeschnitten → abstrahierte **S-Form**
- Schatten in der unteren Hälfte → Räumlichkeit und Tiefe
- Varianten: Positiv / Negativ / 1C Stencil (je)

### Wort-Bild-Marke
- Schrift: **PX Grotesk Bold** (Buchstabe »i« leicht angepasst)
- Abstand Icon ↔ Wortmarke = Breite des Kleinbuchstabens »s«
- **Schutzraum** = Höhe des Buchstabens »s« (ringsum Logo und jedes Element)

---

## 5. Sprach-Shapes

### Konzept
Grafisches Herzstück des Designsystems. Repräsentieren Gesprächstypen und visualisieren Kommunikation.

### 6 Gesprächstypen → 6 Shapes

| Shape | Gesprächstyp |
|-------|-------------|
| Shape 1 | Monolog |
| Shape 2 | Dialog |
| Shape 3 | Geplauder |
| Shape 4 | Besprechung |
| Shape 5 | Verhandlung |
| Shape 6 | (offen) |

### Konstruktionsprinzipien
- Grundform: **Quadrat**
- Elemente: Kreise + Rechtecke mit **zwei einheitlichen Radien**
- Farbe: frei aus **Brand Colors** wählbar (keine feste Zuordnung)

### 3 Anwendungsfälle

| Nr | Anwendung | Ebene |
|----|-----------|-------|
| 01 | Grafische Elemente / Hintergründe | Marketing, Branding, Kampagnen |
| 02 | Maske für Freisteller (Avatar) | Alle Medien |
| 03 | Maske für Fotos | Alle Medien |

### Do's & Don'ts (Shapes als grafische Elemente)
| ✅ Do | ❌ Don't |
|------|---------|
| Brand Colors verwenden | Farbfläche im Hintergrund |
| Einheitliche Größen | Verschiedene, uneinheitliche Größen |
| Symmetrische Anordnung | Asymmetrische Anordnung |
| Dezenter Drop-Shadow bei Interface-Elementen | Kein Drop-Shadow / zu starker Schatten |
| Punktraster im Hintergrund | Farbdoppelungen |

### Do's & Don'ts (Shapes als Masken)
| ✅ Do | ❌ Don't |
|------|---------|
| Freisteller / Bildfokus in visueller Mitte | Motiv aus Mitte versetzt |
| Genug Raum für Motiv | Zu nah rangezoomt |
| Brand Color als Fläche | Verlauf als Fläche |
| Alle wichtigen Bildelemente sichtbar | Gesichter / wichtige Details verdeckt |

---

## 6. Marker-Scribbles

### Konzept
Handschriftliche Annotationen als charmante Ergänzung — nie Pflicht, immer Option.

### Schrift
**Verveine** (Handschrift)

### Einsatzregeln
- Nur wenn **Mehrwert** entsteht (Inhalt oder Optik)
- Haltung: **smart, souverän, supportive** — locker, humorvoll (kein Business-Filter)
- Nur auf **Marketing-, Web- und Kampagnen-Ebene** (nicht im Produkt)
- Größe: angemessen (nicht zu groß!)

---

## 7. Layout-Stilmittel

### Weißraum
- Mindestens **25–50% Negativraum** pro Layout
- Großzügige Headlines, die viel Raum bekommen
- Klare visuelle Hierarchie über Größe und Abstand

### Punktraster
- Dezentes Hintergrund-Pattern für "Tech-Vibes"
- **Grey** (Brand-Ebene) oder **Mint** (Produkt-Ebene)
- Mit Verlauf ausgeblendet wenn Text vorhanden (Richtung frei wählbar)
- Symmetrisch im Layout — nie angeschnitten

### Box-Elemente (5 Typen)

| Variante | Typ | Beschreibung |
|----------|-----|-------------|
| V1 | Image Box | Foto im Kasten |
| V2 | Image Box + Freisteller | Freigestelltes Bild im Kasten |
| V3 | Color Box + Text | Farbige Fläche mit Text |
| V4 | Color Box + Interface | Farbige Fläche mit UI-Element |
| V5 | Gradient Box + Interface | Verlauf-Fläche mit UI-Element |

**Regeln:**
- Einheitliche **Eckenradien** und **Abstände** überall
- Text: linksbündig, oben links, mit Luft
- Interface: mittig, genug Rand, dezenter Schlagschatten
- Keine Farbdopplungen in einer Übersicht

### Bento Grid
- Vorgegebene Fläche → grobes Raster → leicht unregelmäßig gefüllt
- Gleiche Abstände und einheitliche Eckenradien
- Max. eine Farbfläche pro Übersicht
- Shapes-Varianten können Box-Elemente ersetzen
- Marker-Scribbles für Feinschliff

---

## 8. Visual-Baukasten (3-Ebenen-System)

Alle Key-Visuals bestehen aus denselben 3 Ebenen:

### Ebene 1 — Hintergrund

| Variante | Beschreibung |
|----------|-------------|
| V1 Weiß | Einfacher weißer Hintergrund (Standard) |
| V2 Punktraster | Grey oder Mint Punktraster |
| V3 Shapes-Komposition | Mehrfarbige Shapes im Hintergrund |
| V4 50:50 Layout mit Verlauf | Hälfte Bild, Hälfte Farbfläche |

### Ebene 2 — Motiv (Collage)

| Variante | Beschreibung |
|----------|-------------|
| V1 | Freisteller-Shape + Interface-Element |
| V2 | Foto-Shape + Interface-Element |
| V3 | Shapes-Komposition + Interface-Element |
| V4 | Freisteller-Shape + Interface im 50:50 Layout |

### Ebene 3 — Scribble (optional)
Handschriftliche Annotationen für Feinschliff — sparsam einsetzen.

---

## 9. Bildsprache

### Konzept: "Human Warmth meets Smart Tech"
- Keine Stockfotos — echte Menschen in natürlicher Umgebung
- Glaubwürdig, atmosphärisch, nah dran

### 3 Bildtypen

| Typ | Beschreibung |
|-----|-------------|
| 01 Portraits & Teams | Nahaufnahmen, spontan, authentisch; perspektivisch mehr Teams |
| 02 Freisteller | Person auf farbiger Fläche, ggf. Blitzlicht; einfarbige Brand-Color-Kleidung |
| 03 Arbeitssituationen | Natürliche Arbeitsumgebungen, warmes Licht, leichte Körnung |

### Fotostil-Vorgaben
| ✅ Do | ❌ Don't |
|------|---------|
| Warmes, natürliches Licht | Hartes, kühles Blitzlicht |
| Geringe Tiefenschärfe | Hohe Tiefenschärfe |
| Analoge Körnung | Digitale Schärfe |
| Légères Styling, lockere Körpersprache | Professionelle Models, definierte Posen |
| Authentische Persönlichkeiten | Business-Styling |

---

## 10. Animationen

### Prinzipien

| Nr | Prinzip | Bedeutung |
|----|---------|-----------|
| 01 | **Simple** | Klar und einfach, kein unnötiger Schmuck |
| 02 | **Precise** | Handwerkliche Exzellenz, feine Beschleunigungskurven |
| 03 | **Smooth** | Präzise, reibungslos, elegant |

### Shapes-Animationen
- Jede Shape hat eine eigene Dynamik (passend zum Gesprächstyp)
- Zyklus: Kreis → Shape → Kreis
- Einsatz auf Call-Screen als Signature-Branding-Moment
- Als animierte Masken: gekürzter Loop (ohne Transformation)

---

## 11. Messaging / Headlines (Inspiration)

Beispiel-Headlines aus der Exploration:

- *„Weil jedes Gespräch wertvoll ist"*
- *„Das Team telefoniert — wir übernehmen den Rest"*
- *„Damit jedes Wort zählt"*
- *„Smarte Telefonie, effizientere Teams"*
- *„Besser zuhören dank AI"*
- *„Für Kundengespräche, die lange nachklingen"*
- *„Telefonie für die Arbeitswelt von morgen — und übermorgen"*
- *„Kein Anruf geht verloren"*
- *„Direkt verbunden"*
- *„Integriert in deinen Workflow"*
- *„Telefonie, die für Euch mitdenkt"*

---

## 12. Schnell-Referenz CSS Custom Properties

```css
:root {
  /* ─── Typografie ─── */
  --font-primary:  'PX Grotesk', sans-serif;
  --font-headline: 'Tiempos Headline', serif;

  --lh-headline-narrow: 1.10;
  --lh-headline-mid:    1.15;
  --lh-headline-wide:   1.20;
  --lh-body-narrow:     1.20;
  --lh-body-mid:        1.25;
  --lh-body-wide:       1.30;

  /* Schriftmischung: Tiempos immer so skalieren */
  --tiempos-scale:          0.96;
  --tiempos-letter-spacing: 0.02em;

  /* ─── Meta Colors ─── */
  --white:      #FFFFFF;
  --black:      #121212;
  --grey-light: #F7F7F7;
  --grey:       #E7E6E6;
  --grey-dark:  #818885;

  /* ─── Brand Colors ─── */
  --electric-lime: #DEFF00;
  --mint:          #D1E6D1;
  --mint-dark:     #006660;
  --cornflower:    #AABCFF;
  --clementine:    #FF7200;
  --lavender:      #E2D8FF;

  /* ─── Soft Colors (Brand Library) ─── */
  --soft-lime:       #EFFDC1;
  --soft-mint:       #EBF5EB;
  --soft-cornflower: #EDF2FF;
  --soft-clementine: #FFEADC;
  --soft-lavender:   #F3F0FF;

  /* ─── Alternate Colors (Brand Library) ─── */
  --electric-cornflower:    #274FF0;
  --electric-lavender:      #8642FE;
  --electric-lavender-dark: #322372;

  /* ─── Interface Colors (UI-Palette) ─── */
  --electric-lime-light: #F2FFB1;
  --electric-lime-dark:  #202500;

  --mint-light: #F1F7F7;
  /* --mint / --mint-dark: s. Brand Colors */

  --cornflower-light: #D6DFFF;
  --cornflower-dark:  #315DFF;

  --clementine-light: #FFDBBD;
  --clementine-dark:  #723401;

  --lavender-mid:  #C1ABFF;
  --lavender-dark: #342364;

  /* ─── States Colors ─── */
  --green:        #00BD82;
  --green-light:  #CCF2E6;
  --green-dark:   #006660;

  --warning:       #FFBD00;
  --warning-light: #FFF2CC;
  --warning-dark:  #866300;

  --error:        #FF5E5E;
  --error-light:  #FFDFDF;
  --error-dark:   #952314;

  /* ─── Gradients (Monochrome) ─── */
  --gradient-electric-lime: linear-gradient(to bottom, #DEFF00 0%, #EFFDC1 50%, #EBF5EB 75%, #EDF2FF 100%);
  --gradient-clementine:    linear-gradient(to bottom, #FF7200 0%, #F3F0FF 100%);
  --gradient-mint-dark:     linear-gradient(to bottom, #006660 0%, #EDF2FF 100%);
  --gradient-cornflower:    linear-gradient(to bottom, #AABCFF 0%, #EDF2FF 100%);
  --gradient-lavender:      linear-gradient(to bottom, #E2D8FF 0%, #F3F0FF 100%);

  /* ─── Gradients (Duochrome) ─── */
  --gradient-cornflower-lavender:  linear-gradient(to bottom, #AABCFF 0%, #E2D8FF 100%);
  --gradient-lavender-clementine:  linear-gradient(to bottom, #E2D8FF 0%, #FFDBBD 100%);
  --gradient-cornflower-mint:      linear-gradient(to bottom, #AABCFF 0%, #D1E6D1 20%, #EBF5EB 100%);

  /* ─── Gradients (Soft) ─── */
  --gradient-soft-lime-cornflower: linear-gradient(to bottom, #EFFDC1 0%, #EDF2FF 100%);
  --gradient-soft-lime-lavender:   linear-gradient(to bottom, #EFFDC1 0%, #F3F0FF 30%, #E2D8FF 100%);
}
```

---

## 13. Live-Referenz: sipgate AI Flow Landing Page

**URL:** https://www.sipgate.de/lp/ai-flow

Aktuelle Produktionsseite, die das Designsystem in der Praxis zeigt. Als primäre visuelle Referenz beim Entwickeln neuer Seiten verwenden.

### Beobachtete Design-Umsetzung

| Element | Umsetzung |
|---------|-----------|
| Hintergrund | Schwarz (`#121212`) und Weiß als dominante Flächen |
| Primärakzent | Electric Lime (`#DEFF00`) für CTAs und Highlights |
| Navigation | Sticky, mit Hover-Underline-Effekt (skalierender gelber Unterstrich) |
| Hero | Großformatiges Visual + Headline + primärer CTA |
| Feature-Blöcke | 4-spaltig, Icons + kurze Beschreibungen |
| Code-Beispiele | Tabbares Interface (Python, Node.js, Go, Ruby) mit Copy-Button |
| Use-Cases | 2-Spalten-Layout mit CTA je Szenario |
| Logos | Marquee-Animation (pausiert bei Hover) |
| Bildsprache | Abstrakte Netzwerkgrafiken, UI-Screenshots, Mitarbeiterfotos |
| Modals | Signup via Iframe-Overlay |

### CTAs auf der Referenzseite
- „Direkt zur API Doku" (primär, Hero)
- „Demo buchen" / „Beraten lassen"
- „Termin vereinbaren"
- „Kostenlos testen"

### Abweichungen vom Styleguide (beachten)
- Violett (`#603DFF`) als zusätzlicher Akzent — entspricht dem **Electric Lavender** (`#8642FE`) / Alternate-Palette aus der Brand Library
- Nicht alle Sprach-Shapes sichtbar — Seite ist tech-/API-fokussiert, nutzt eher abstrakte Netzwerk-Grafiken

---

## 15. Navigationspunkte / Sitemap-Vorlage (Marketing)

Basierend auf den Interface-Explorationen:

```
- Produkt
- Für Unternehmen
- Preise
- Info-Center
  - Blog
  - Webinare
- [CTA] Kostenlos testen
- [CTA] Vertrieb kontaktieren
```

---

*Quellen: Neo PBX Living Styleguide (August 2024) + NeoBrandLibrary — destilliert für den praktischen Einsatz in der Web-Entwicklung.*

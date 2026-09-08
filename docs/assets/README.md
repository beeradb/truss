# truss — logo assets

The word sits on a braced span; the icon is that span squared up. Both are drawn as
assemblies — hollow chords, solid braces, one bolt centred in each corner of the frame,
last brace carrying the accent. The long rule under the wordmark adds posts at the panel
points (a Warren truss with verticals); the icon leaves them out, because at that width
they crowd the diagonals.

Wordmark is **IBM Plex Sans 700**, converted to outlines — the SVGs carry no font
dependency and render identically everywhere.

## Colours

| role            | hex       | notes                                     |
|-----------------|-----------|-------------------------------------------|
| ink             | `#2a2724` | wordmark, chords, braces, bolts           |
| accent (clay)   | `#a8543a` | the last brace only                       |
| accent on dark  | `#c9714f` | lifted for contrast on ink backgrounds    |
| paper           | `#f4efe6` | background; mark colour on dark           |

## Which file

| file                        | use                                                     |
|-----------------------------|---------------------------------------------------------|
| `truss-lockup.svg`          | primary — word over the span. README, docs and site header |
| `truss-horizontal.svg`      | icon + word, where vertical room is tight (nav)          |
| `truss-wordmark.svg`        | word alone                                               |
| `truss-mark.svg`            | icon alone, 48px and up                                  |
| `truss-mark-small.svg`      | icon at 32px and below — bolts gone, braces reduced to a V |
| `favicon.svg`               | same as `truss-mark-small.svg`                           |
| `*-mono.svg`                | single colour via `currentColor` — inherits text colour  |
| `*-ondark.svg`              | for ink backgrounds                                      |
| `png/`                      | raster fallbacks; `favicon-16/32/64`, mark 256/512       |

## Rules

- **Below 48px use `truss-mark-small.svg`.** The corner bolts close up first, then the
  inner braces; the frame and a single V carry the shape on their own.
- **Clear space**: the depth of the frame band on every side. For the lockup, the height
  of the span rule.
- The frame is two stroked rectangles with nothing filled between them, so the mark sits
  on any background without carrying its own paper colour.
- **One brace is coloured, and it is always the last one.** Don't colour a second, don't
  move it, don't recolour it to a status colour — it means the gate that closed last.
- The span rule in the lockup is drawn to the width of the word. If you reset the wordmark
  at another size, the rule scales with it — don't stretch one without the other.
- `*-mono.svg` uses `currentColor`, so inlining it in HTML or a README picks up the
  surrounding text colour, including in dark mode.

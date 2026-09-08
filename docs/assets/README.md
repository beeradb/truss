# truss — logo assets

The word sits on a single Warren span; the icon is a king-post gable truss.
Wordmark is **IBM Plex Sans 700**, converted to outlines — the SVGs carry no
font dependency and render identically everywhere.

## Colours

| role            | hex       | notes                                  |
|-----------------|-----------|----------------------------------------|
| ink             | `#2a2724` | wordmark, chords, outer triangle       |
| accent (clay)   | `#a8543a` | king post; last member of the span     |
| accent on dark  | `#c9714f` | lifted for contrast on ink backgrounds |
| paper           | `#f4efe6` | background; mark colour on dark        |

## Which file

| file                        | use                                                    |
|-----------------------------|--------------------------------------------------------|
| `truss-lockup.svg`          | primary — word over the span. README, docs headers, site header |
| `truss-horizontal.svg`      | icon + word, where vertical room is tight (nav)        |
| `truss-wordmark.svg`        | word alone                                              |
| `truss-mark.svg`            | icon alone, 32px and up                                 |
| `truss-mark-small.svg`      | icon at 24px and below — struts dropped                 |
| `favicon.svg`               | same as `truss-mark-small.svg`                          |
| `*-mono.svg`                | single colour via `currentColor` — inherits text colour |
| `*-ondark.svg`              | for ink backgrounds                                     |
| `png/`                      | raster fallbacks; `favicon-16/32/64`, mark 256/512      |

## Rules

- **Below 32px use `truss-mark-small.svg`.** The struts silt up; the outer
  triangle and king post carry the shape on their own.
- **Clear space**: one king-post width on every side of the mark. For the
  lockup, the height of the span rule.
- The span rule in the lockup is drawn to the width of the word. If you reset
  the wordmark at a different size, the rule scales with it — don't stretch one
  without the other.
- Don't recolour the king post to anything but the clay accent, and don't
  add a second accent. One member is coloured because one gate closes last.
- `*-mono.svg` uses `currentColor`, so inlining it in HTML or a README picks up
  the surrounding text colour, including in dark mode.

# gpxtiler

gpxtiler loads the OpenStreetMap GPS traces into PostGIS, keeps them up to date and serves them as PNG tiles.


```sh
# once: load the GPX dump
gpxtiler import -expire-dir /data/expire gpx-latest.gpx.bz2

# every few minutes: new traces from the RSS feed of the website
gpxtiler update -rss -site https://www.openstreetmap.org -expire-dir /data/expire

# daily: sync with the list of identifiable trace ids (delete, download the missing ones)
gpxtiler update -ids https://planet.openstreetmap.org/gps/gpx_ids/identifiable.txt -site https://www.openstreetmap.org -expire-dir /data/expire

# PNG tiles on /lines/{z}/{x}/{y}.png. A tile is read from -tile-dir, or drawn and
# kept there for the next request. Above -cache-zoom it is drawn on every request
gpxtiler serve -tile-dir /data/tiles -cache-zoom 14

# once before opening, and daily: draw all the tiles of zoom 0 to 9 to disk
gpxtiler render -zooms 0-9 -tile-dir /data/tiles

# every few minutes: apply the expire files written by import and update
# (redraw the expired tiles of zoom 6 to 9, remove the expired ones of zoom 10 to 14 from disk)
gpxtiler render -expired -zooms 6-9 -expire-dir /data/expire -tile-dir /data/tiles
```

`-site` is `https://www.openstreetmap.org` by default. Without `-expire-dir` no expire files are written. Run `gpxtiler <command> -h` to see all the flags.

## How it works

- `import` reads the GPX dump from [planet-dump-ng](https://github.com/zerebubuth/planet-dump-ng) (file, URL or `-` for stdin) and stores each trace as lines in PostGIS, plus a simplified copy per low zoom (`-simplified-zooms 5,8,11`), like Spyglass does.
- `update -rss` reads the website RSS feed and downloads the new traces, like [gpx-updater](https://github.com/openstreetmap/gpx-updater) does today.
- `update -ids` reads the daily list of identifiable trace ids (openstreetmap/chef#881), deletes the traces that are not in it and downloads the missing ones.
- `serve` draws the tiles from PostGIS. The drawing is a port of [datamaps](https://github.com/ericfischer/datamaps), so the tiles look like gps-tile.openstreetmap.org: colors by direction, GPS jumps dimmed. `/` is a small map to look at them.
- `render` keeps the low zoom tiles on disk, like ops does with Tilekiln on vector.openstreetmap.org.

## Two ideas to know

**1. The lines are stored four times, with less detail each time.** `import` keeps the full lines in `trace_segments` and a simplified copy for zoom 11, 8 and 5 (`trace_segments_z11`, `_z8`, `_z5`): at zoom 5 a trace of 10,000 points is 20 pixels long, so the copy keeps about 20 points. A tile is always drawn from the copy closest to its zoom, so the low zooms do not read millions of points.

**2. A tile is drawn once and kept on disk.** `serve` reads a tile from `-tile-dir`; when it is not there, it draws it and keeps it there if the zoom is 14 or less. Above that a tile is small and fast, so it is drawn on every request. A tile of zoom 0 to 9 covers a big area and is slow to draw, so `render -zooms 0-9` draws all of them in advance; without it, the first request of each one is slow.

| Zoom | Drawn by | Kept on disk |
|---|---|---|
| 0 to 9 | `render` in advance, or `serve` on the first request | yes |
| 10 to 14 | `serve` on the first request | yes |
| 15 to 20 | `serve` on every request | no |

## Expire files

When a trace is added or deleted, the tiles that show it are out of date. `import` and `update` write them to `-expire-dir/expire-<time>.txt`, one tile per line:

```
8/129/87
9/259/175
...
```

It is the format of the osm2pgsql expire files, so the same file works for the PNG tiles and for Tilekiln, like ops does on vector.openstreetmap.org:

- `render -expired -zooms 6-9` redraws the listed tiles of zoom 6 to 9, removes the listed ones of zoom 10 to 14 from disk (`serve` draws them again on the next request) and deletes the file. Zoom 0 to 5 wait for the daily `render -zooms 0-5`.
- `tilekiln generate tiles --config config.yaml < expire-<time>.txt` regenerates the stored vector tiles (when Tilekiln runs with storage).

The files list zoom 0 to 14 (`-expire-min-zoom`, `-expire-max-zoom`): the zooms that are kept on disk. Above `-cache-zoom` nothing is stored, so nothing needs to expire.

Vector tiles from the same tables: [gpx-vector-tiles](https://github.com/Rub21/gpx-vector-tiles) (Tilekiln config). To run everything with Docker on a server: [gpx-tiles-stack](https://github.com/Rub21/gpx-tiles-stack).

## Development

You do not need Go on your machine.

```
docker compose up -d db                  # PostGIS on localhost:54329
docker compose run --rm test             # run all tests
```

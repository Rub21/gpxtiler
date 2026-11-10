# gpxtiler

gpxtiler loads the OpenStreetMap GPS traces into PostGIS, keeps them up to
date and serves them as PNG tiles.

```
gpxtiler import gpx-latest.gpx.bz2   # once: load the GPX dump
gpxtiler update -rss                 # every few minutes: new traces
gpxtiler update -ids LIST            # daily: remove deleted traces
gpxtiler serve                       # PNG tiles on /lines/{z}/{x}/{y}.png
gpxtiler render -all                 # once, and daily: draw the low zoom tiles to disk
gpxtiler render                      # every few minutes: apply the expire files
```

Run `gpxtiler <command> -h` to see the flags.

## How it works

- `import` reads the GPX dump from
  [planet-dump-ng](https://github.com/zerebubuth/planet-dump-ng) (file, URL
  or `-` for stdin) and stores each trace as lines in PostGIS, plus a
  simplified copy per low zoom (`-low-zooms 5,8,11`), like Spyglass does.
- `update -rss` reads the website RSS feed and downloads the new traces,
  like [gpx-updater](https://github.com/openstreetmap/gpx-updater) does today.
- `update -ids` reads the daily list of identifiable trace ids
  (openstreetmap/chef#881), deletes the traces that are not in it and
  downloads the missing ones.
- `serve` draws the tiles from PostGIS. The drawing is a port of
  [datamaps](https://github.com/ericfischer/datamaps), so the tiles look like
  gps-tile.openstreetmap.org: colors by direction, GPS jumps dimmed.
  `/` is a small map to look at them.
- `render` keeps the tiles on disk, like ops does with Tilekiln on
  vector.openstreetmap.org.

Tiles by zoom (the numbers are flags):

| Zoom | Table | PNG |
|---|---|---|
| 0 to 9 | simplified copies | pre-rendered by `render -all`, daily |
| 10 to 14 | copy for 11, then full | drawn once by `serve`, kept on disk |
| 15 to 20 | full | drawn on every request |

`import` and `update` write the changed tiles to expire files, one `z/x/y`
per line for zooms 0 to 14, the same format as osm2pgsql. `render` reads
them every few minutes: it redraws the expired pre-rendered tiles from
zoom 6 up and drops the expired cached tiles. Tilekiln can read the same
files with `tilekiln generate tiles`.

## Vector tiles with Tilekiln

[Tilekiln](https://github.com/pnorman/tilekiln) is the vector tile server
ops runs on vector.openstreetmap.org. The `tilekiln/` folder has its config
and the SQL of the `traces` layer: lines cut by direction of travel
(`dir`, 32 steps) with a `fade` value for GPS jumps, read from the same
tables. `compose.server.yaml` runs it with `serve dev`, drawing on the fly.
The TileJSON is on `/gps/tilejson.json`.

## Run on a server

```
cp .env.example .env        # set the password and the URLs
docker compose -f compose.server.yaml up -d db
docker compose -f compose.server.yaml run --rm import       # once
docker compose -f compose.server.yaml run --rm render-all   # once
docker compose -f compose.server.yaml up -d                 # serve, updates, render
```

## Development

You do not need Go on your machine.

```
docker compose up -d db                  # PostGIS on localhost:54329
docker compose run --rm test             # run all tests
```

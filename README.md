# gpxtiler

gpxtiler loads the OpenStreetMap GPS traces into PostGIS, keeps them up to
date and serves them as PNG tiles.

```
gpxtiler import gpx-latest.gpx.bz2   # once: load the GPX dump
gpxtiler update -rss                 # every few minutes: new traces
gpxtiler update -ids LIST            # daily: remove deleted traces
gpxtiler serve                       # PNG tiles on /lines/{z}/{x}/{y}.png
```

Run `gpxtiler <command> -h` to see the flags.

## How it works

- `import` reads the GPX dump from
  [planet-dump-ng](https://github.com/zerebubuth/planet-dump-ng) (file, URL
  or `-` for stdin) and stores each trace as lines in PostGIS.
- `update -rss` reads the website RSS feed and downloads the new traces,
  like [gpx-updater](https://github.com/openstreetmap/gpx-updater) does today.
- `update -ids` reads the daily list of identifiable trace ids
  (openstreetmap/chef#881), deletes the traces that are not in it and
  downloads the missing ones.
- `serve` draws the tiles on the fly from PostGIS. The drawing is a port of
  [datamaps](https://github.com/ericfischer/datamaps), so the tiles look like
  gps-tile.openstreetmap.org: colors by direction, GPS jumps dimmed.
  `/` is a small map to look at them.

`import` and `update` write the changed tiles to expire files, for the
future `render` step.

## Run on a server

```
cp .env.example .env        # set the password and the URLs
docker compose -f compose.server.yaml up -d db
docker compose -f compose.server.yaml run --rm import   # once
docker compose -f compose.server.yaml up -d             # serve and updates
```

## Development

You do not need Go on your machine.

```
docker compose up -d db                  # PostGIS on localhost:54329
docker compose run --rm test             # run all tests
```

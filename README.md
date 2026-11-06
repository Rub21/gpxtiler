# gpxtiler

gpxtiler loads the OpenStreetMap GPX dump from planet-dump-ng into PostGIS
and will serve the GPS traces tiles.

```
gpxtiler import gpx-latest.gpx.bz2   # each time there is a new dump
gpxtiler update -rss                 # timer, every few minutes: new traces
gpxtiler serve                       # not ready yet
gpxtiler render                      # not ready yet
```

## import

`import` reads the dump as a stream and syncs the database with it:

- new trace id: insert the trace and its lines
- known id: update name, description and tags (the geometry does not change)
- id not in the dump: the trace was deleted or made private, so delete it

Each `<trkseg>` becomes 2D LineStrings in EPSG:3857. A line is cut every
1000 points or 50 km and at ±180°. It also fills `trace_segments_low`, a
simplified copy for low zooms.

Each run writes the tiles of inserted and deleted traces to an expire file
(`-expire-dir`), one `z/x/y` per line. The first load writes none.

For big files, use lbzip2, it is much faster than Go's bzip2:

```
lbzip2 -dc gpx-latest.gpx.bz2 | gpxtiler import -
```

Run `gpxtiler import -h` to see all the flags.

## update -rss

`update -rss` loads new traces between two dumps, like gpx-updater does
today:

1. read `/traces/rss` on the website to get the highest trace id
2. check every id after the last one checked, up to that id
3. download `/traces/:id/data.gpx` and insert the trace; the website
   answers 404 for traces that are deleted or not public, and unpacks
   gz, bz2, zip and tar uploads

It only inserts. Deleted traces are removed by the next `import`. The RSS
mode has no name, description or tags, because the website only gives
them through the API with a login; the next dump adds them.

The last checked id is kept in `rss_state`. On the first run it starts
after the highest id in the database, or after `-start-id`. A server
error stops the run, and the next run tries the same id again. Each run
checks at most `-max-traces` ids (1000) for at most `-max-time` (10m),
with `-delay` (500ms) between requests.

`import` and `update` take a PostgreSQL advisory lock, so only one runs at
a time. If `update` finds the lock taken, it exits without error.

## Development with Docker

You do not need Go on your machine.

```
docker compose up -d db                  # PostGIS 3.5
docker compose run --rm test             # build and run all tests
docker compose build gpxtiler
docker compose run --rm gpxtiler import /data/gpx-latest.gpx.bz2
```

Put dump files in `./data`. The database is on `localhost:54329`
(user, password and database: `gpxtiler`).

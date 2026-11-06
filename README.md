# gpxtiler

gpxtiler loads the OpenStreetMap GPX dump from planet-dump-ng into PostGIS
and will serve the GPS traces tiles.

```
gpxtiler import gpx-latest.gpx.bz2   # each time there is a new dump
gpxtiler update -rss                 # timer, every few minutes: new traces
gpxtiler update -ids LIST            # timer, daily: deleted traces
gpxtiler serve                       # not ready yet
gpxtiler render                      # not ready yet
```

## import

`import` reads the dump as a stream and syncs the database with it:

- new trace id: insert the trace and its lines
- known id: update name, description and tags (the geometry does not change)
- id not in the dump: the trace was deleted or is no longer identifiable,
  so delete it. Ids higher than the highest id in the dump are kept,
  because they are newer than the dump and came from `update`.

An empty dump is an error, and so is a dump that would delete more than
`-max-delete-ratio` (50 %) of the traces.

Each `<trkseg>` becomes 2D LineStrings in EPSG:3857. A line is cut every
1000 points or 50 km and at ±180°. It also fills `trace_segments_low`, a
simplified copy for low zooms.

Each run writes the tiles of inserted and deleted traces to an expire file
(`-expire-dir`), one `z/x/y` per line. The first load writes none.

For big files, use lbzip2, it is much faster than Go's bzip2:

```
lbzip2 -dc gpx-latest.gpx.bz2 | gpxtiler import -
```

`import` also reads from a URL:

```
gpxtiler import https://planet-gpx-tracks.rub21.com/gps/gpx-latest.gpx.bz2
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

It only inserts. Deleted traces are removed by `update -ids` or the next
`import`. The RSS
mode has no name, description or tags, because the website only gives
them through the API with a login; the next dump adds them.

The last checked id is kept in `rss_state`. On the first run it starts
after the highest id in the database, or after `-start-id`. A server
error stops the run, and the next run tries the same id again. Each run
checks at most `-max-traces` ids (1000) for at most `-max-time` (10m),
with `-delay` (500ms) between requests.

## update -ids

`update -ids` syncs the database with the daily list of identifiable trace
ids, `gpx_ids/identifiable.txt` on planet.openstreetmap.org
(openstreetmap/chef#881). The list is a file or a URL, one id per line:

```
gpxtiler update -ids https://planet.openstreetmap.org/gpx_ids/identifiable.txt
```

- id in the database and not in the list: the trace was deleted or is no
  longer identifiable, so delete it
- id in the list and not in the database: download it, like `update -rss`

Ids higher than the highest id in the list are kept, because they are newer
than the list. It stops before any change if the list would delete more
than `-max-delete-ratio` (50 %) of the traces, or if the list has a bad
line. Missing traces over `-max-traces` or `-max-time` wait for the next
run.

## Lock

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

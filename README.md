# gpxtiler

gpxtiler loads the OpenStreetMap GPX dump from planet-dump-ng into PostGIS
and will serve the GPS traces tiles.

```
gpxtiler import gpx-latest.gpx.bz2   # each time there is a new dump
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

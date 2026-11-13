-- Tables shared by import (writes) and serve/render (read).
-- All geometries are 2D, in Web Mercator (EPSG:3857).

CREATE TABLE IF NOT EXISTS traces (
    gpx_id      bigint PRIMARY KEY,
    name        text,
    description text
);

CREATE TABLE IF NOT EXISTS trace_tags (
    gpx_id bigint,
    tag    text,
    PRIMARY KEY (gpx_id, tag)
);

-- One row per piece of a <trkseg>, partitioned by year (see importer.EnsureYears).
-- year: the GPS year of the first point when it is possible, else the upload
-- year. mode: transport from the median speed, 0 unknown, 1 walk, 2 bike,
-- 3 vehicle, 4 plane. The primary key has year because the partition key
-- must be part of every unique key.
CREATE TABLE IF NOT EXISTS trace_segments (
    gpx_id     bigint,
    trackid    int,                        -- position of the <trkseg> in the trace
    segment    int,                        -- piece inside the trackid
    year       smallint NOT NULL,
    mode       smallint NOT NULL,
    geom       geometry(LineString, 3857),
    started_at timestamptz,
    ended_at   timestamptz,
    PRIMARY KEY (gpx_id, trackid, segment, year)
) PARTITION BY RANGE (year);

-- The simplified copies for the low zooms, trace_segments_z<N>, and the
-- partitions per year of every table are created by import (see
-- importer.EnsureLowTables and importer.EnsureYears).

-- Only one row: the <time> of the last dump that was fully loaded.
CREATE TABLE IF NOT EXISTS dump_state (
    id        boolean PRIMARY KEY DEFAULT true CHECK (id),
    timestamp timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS trace_segments_geom_idx ON trace_segments USING gist (geom);

-- Only one row: the last trace id checked by "update --rss".
CREATE TABLE IF NOT EXISTS rss_state (
    id      boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_id bigint NOT NULL
);

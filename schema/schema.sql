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

CREATE TABLE IF NOT EXISTS trace_segments (
    gpx_id     bigint,
    trackid    int,                        -- position of the <trkseg> in the trace
    segment    int,                        -- piece inside the trackid
    geom       geometry(LineString, 3857),
    started_at timestamptz,
    ended_at   timestamptz,
    PRIMARY KEY (gpx_id, trackid, segment)
);

-- The simplified copies for the low zooms, trace_segments_z<N>, are created
-- by import (see importer.EnsureLowTables).

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

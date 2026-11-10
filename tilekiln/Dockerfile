# Tilekiln, the vector tile server ops uses on vector.openstreetmap.org.
FROM python:3.12-slim
RUN pip install --no-cache-dir "tilekiln==0.8.4" "psycopg[binary]"
ENTRYPOINT ["tilekiln"]

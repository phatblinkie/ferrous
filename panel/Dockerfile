FROM python:3.12-slim

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY . .

ENV FERROUS_DB=/data/ferrous.db \
    PANEL_BIND=0.0.0.0 \
    PANEL_PORT=8122

VOLUME /data
EXPOSE 8122

# gthread workers; --timeout 0 keeps long-lived SSE streams alive
CMD ["gunicorn", "--workers", "2", "--threads", "16", "--timeout", "0", \
     "--bind", "0.0.0.0:8122", "--access-logfile", "-", "server:app"]

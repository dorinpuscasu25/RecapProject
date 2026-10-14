# Behind a TLS-inspecting proxy: docker build --secret id=extra_ca,src=/path/ca.pem services/ml
FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 ML_CACHE_DIR=/models
WORKDIR /srv
COPY requirements.txt .
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then cat /etc/ssl/certs/ca-certificates.crt /run/secrets/extra_ca > /tmp/ca.pem; export PIP_CERT=/tmp/ca.pem; fi; \
    pip install -r requirements.txt && rm -f /tmp/ca.pem
COPY app ./app
RUN useradd -u 10001 app && mkdir -p /models && chown app /models
USER app
EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/healthz',timeout=3).status==200 else 1)"
CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]

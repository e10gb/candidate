FROM python:3.12-slim
RUN pip install --no-cache-dir nats-py
WORKDIR /app
COPY hedger.py .
ENV NATS_URL=nats://nats:4222
CMD ["python", "-u", "hedger.py"]

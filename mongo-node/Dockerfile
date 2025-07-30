FROM python:3.11-slim
WORKDIR /app
COPY server.py .
RUN pip install pymongo
CMD ["python", "server.py"]
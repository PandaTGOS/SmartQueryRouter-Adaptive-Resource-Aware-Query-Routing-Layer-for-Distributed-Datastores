FROM python:3.11-slim
WORKDIR /app
COPY test_queries.py .
RUN pip install requests
CMD ["python", "test_queries.py"]
import requests
import json
import random
import time

URL = "http://router:8080/query"
TYPES = ['read', 'write']

def random_query():
    return {
        "type": random.choice(TYPES),
        "key": f"key-{random.randint(1, 100)}",
        "value": f"value-{random.randint(1000, 9999)}"
    }

while True:
    try:
        q = random_query()
        requests.post(URL, data=json.dumps(q))
    except Exception as e:
        print("Query error:", e)
    time.sleep(0.3)

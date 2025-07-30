import socket
import threading
import os
import pymongo

MONGO_URI = os.getenv("MONGO_URI", "mongodb://localhost:27017")
client = pymongo.MongoClient(MONGO_URI)

def handle_client(conn):
    try:
        db = client.testdb
        result = db.testcol.find_one() or {"msg": "empty"}
        msg = str(result).encode()
        conn.sendall(msg)
    except Exception as e:
        conn.sendall(f"Error: {e}".encode())
    finally:
        conn.close()

def start_server():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.bind(("0.0.0.0", 9001))
    server.listen(5)
    print("Mongo proxy listening on port 9001")
    while True:
        client_sock, _ = server.accept()
        threading.Thread(target=handle_client, args=(client_sock,)).start()

if __name__ == "__main__":
    start_server()
// mongo-init.js
db = db.getSiblingDB("testdb");
db.testcol.insertMany([
  { name: "Alpha", value: 42 },
  { name: "Beta", value: 84 },
  { name: "Gamma", value: 126 }
]);
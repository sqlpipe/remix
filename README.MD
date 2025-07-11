# Remix by SQLpipe

Remix is a free, open-source data streaming solution, written in Golang. It has three main objectives:

- Replicate data from one system to another, in real time, in a flexible way.
- Facilitate the curation of high quality AI training datasets.
- Encourage the creation of canonical data models that can be enforced across your organization.

**Data Replication**

Remix features a "fan-in, fan-out" architecture when replicating data. It pulls in data, transforms it to fit into predefined models that you define, and puts objects in a queue. Then, for each system that you want to push to, it transforms the objects to a format that the target system will accept, and pushes to it.

We call the process of transforming the data "remixing". Right now, remixing simply renames fields, changes data types, and builds [idempotent](https://en.wikipedia.org/wiki/Idempotence) [upsert](https://en.wiktionary.org/wiki/upsert) / delete commands.

Just remember, data is remixed on the way in to fit your canonical data models, then remixed on the way out to be accepted by target systems.

**AI Dataset Curation**

AI thrives on clean, standardized datasets. Remix facilitates the creation of those datasets by forcing you to define data models in a standardized way.

Once you've defined those models, Remix replicates data (which conforms to those models) to any number of storage systems. Depending on your needs, you might replicate data to traditional databases, a data warehouse, vector databases, or even systems used to serve large-scale AI training runs like VAST, Databricks, or S3.

**Canonical Data Model Enforcement**

Models and remixing logic are defined with [JSON Schema](https://json-schema.org) and YAML, respectively. This declarative nature makes it easy to keep your models and transformation logic in source control, such as Github, and works great with automated deployment systems like Terraform or Ansible.

JSON Schema is the most widely accepted format for defining shared, canonical data models. Because the hardest part about data modeling is getting everyone to agree and conform, it's important to use a popular, interchangeable format with a good tool ecosystem. [JSON Schema's tool ecosystem](https://json-schema.org/tools) is unmatched, and popular use cases include:

- YAML to JSON and back converters
- Endless validator tools for every possible language / runtime
- Schema to data translators / data to schema translators
- Schema to code translators / code to schema translators
- Auto documentation tools
- Integration into popular data systems like [PostgreSQL](https://github.com/supabase/pg_jsonschema), [Kafka](https://docs.confluent.io/platform/current/schema-registry/fundamentals/serdes-develop/serdes-json.html), [MongoDB](https://www.mongodb.com/resources/languages/json-schema-examples), and many others.

We use JSON Schema to validate models in Remix because (1) of course there is already a [high-quality tool](https://github.com/santhosh-tekuri/jsonschema) to do that and (2) the format's portability enables you to re-use those models in other systems.

## Replication Algorithm Summary

1. Watch (or listen) for data changes by querying [change data capture](https://en.wikipedia.org/wiki/Change_data_capture) (CDC) endpoints, or receiving [webhooks](https://en.wikipedia.org/wiki/Webhook) on an API endpoint.
2. "Remixing" the data that comes in from those sources into predefined models (defined via [JSON Schema](https://json-schema.org)), and placing those validated model objects in a queue, or some fast external data storage system, like Redis.
3. According to rate limits that you control, objects in the queue are remixed again and [upserted](https://en.wiktionary.org/wiki/upsert) to, or deleted in, target systems that you define.

## Development Status / Roadmap

Remix is a new tool and is being actively developed.

Would you like a certain integration or feature built? SQLpipe, the company behind Remix, offers [service packages]() that allow you to influence the roadmap. Turnaround time can be as fast as a few weeks.

### Supported Integrations

- PostgreSQL
- Stripe

### Integrations to be added

#### AI / Blob Storage / Data Lake

- VAST Data
- Scale AI
- Spark / Databricks
- Blob storage (S3, Google Cloud Storage, Azure Blob Storage)
- Iceberg

#### Databases / Data Warehouses

- MySQL
- SQL Server
- Snowflake
- Bigquery

#### Other

- Arbitrary API endpoints
- Kafka (send validated objects to your existing message broker)
- Kinesis
- AWS SQS
- RabbitMQ

### Distribution / Kubernetes

Currently, Remix is single-node software with no outside data storage dependencies. However, it has been designed in a way that facilitates being deployed in a distributed fashion, eg with Kubernetes.

As of right now, it keeps active, validated objects in a queue in RAM. There are two additional storage / cooperation features that will be added:

- The ability to write objects to disk, thus making a single-node system resilient to hardware failures.
- The ability to offload storage to Redis, thus making a distributed setup quite easy. At that point, you will be able to drop Remix into Kubernetes, scale the amount of nodes up and down according to your compute needs, and have them cooperate using Redis as a central communication hub.

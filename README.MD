# Remix by SQLpipe

Remix is a free, open-source data streaming solution, written in Golang. It has three main objectives:

- Replicate data from one system to another, in real time, in a flexible way.
- Facilitate the curation of high quality AI training datasets.
- Encourage the creation of canonical data models that can be enforced across your organization.

### Data Replication

Remix features a "fan-in, fan-out" architecture when replicating data. It pulls in data, transforms it to fit into predefined models that you define, and puts objects in a queue. Then, for each system that you want to push to, it transforms the objects to a format that the target system will accept, and pushes to it.

We call the process of transforming the data "remixing". Right now, remixing simply renames fields, changes data types, and builds [idempotent](https://en.wikipedia.org/wiki/Idempotence) [upsert](https://en.wiktionary.org/wiki/upsert) / delete commands.

Just remember, data is remixed on the way in from a system to fit your canonical data models, then remixed again on the way out to be accepted by other systems.

### AI Dataset Curation

AI thrives on clean, standardized datasets. Remix facilitates the creation of those datasets by forcing you to define data models in a standardized way.

Once you've defined those models, Remix replicates data (which conforms to those models) to any number of storage systems. Depending on your needs, you might replicate data to traditional databases, a data warehouse, vector databases, or even systems used to serve large-scale AI training runs like VAST, Databricks, or S3.

### Canonical Data Model Enforcement

Models and remixing logic are defined with [JSON Schema](https://json-schema.org) and YAML, respectively. This declarative nature makes it easy to keep your models and transformation logic in source control, such as Github, and works great with automated deployment systems like Terraform or Ansible.

JSON Schema is the most widely accepted format for defining shared, canonical data models. Because the hardest part about data modeling is getting everyone to agree and conform, it's important to use a popular, interchangeable format with a good tool ecosystem. [JSON Schema's tool ecosystem](https://json-schema.org/tools) is unmatched, and popular use cases include:

- YAML to JSON and back converters
- Endless validator tools for every possible language / runtime
- Schema to data translators / data to schema translators
- Schema to code translators / code to schema translators
- Auto documentation tools
- Integration into popular data systems like [PostgreSQL](https://github.com/supabase/pg_jsonschema), [Kafka](https://docs.confluent.io/platform/current/schema-registry/fundamentals/serdes-develop/serdes-json.html), [MongoDB](https://www.mongodb.com/resources/languages/json-schema-examples), and many others.

We use JSON Schema to validate models in Remix because (1) of course there is already a [high-quality tool](https://github.com/santhosh-tekuri/jsonschema) to do that and (2) the format's portability enables you to re-use those models in other systems.

### Replication Algorithm Summary

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

## Understand Remix

Remix requires you to understand its declarative data mapping paradigm. This takes a few minutes, and is best explained in combination with an example.

You interact with remix through two types of configuration files:

- [JSON Schema](https://json-schema.org/learn/getting-started-step-by-step)
- Remix files.

Template files for popular use cases will be built over time in the `/examples` directory of this repository. For now, let's pick a concrete example and examine how to use Remix to sync product data between Stripe and PostgreSQL.

First, the schema file:

### Example Product Schema

```json
{
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "title": "product",
    "type": "object",
    "properties": {
        "id": {
            "type": ["number", "null", "string"]
        },
        "stripe_id": {
            "type": ["string", "null"]
        },
        "name": {
            "type": "string"
        },
        "price": {
            "type": ["number", "null"]
        }
    }
}
```

Here we are defining a schema "product" with four fields:

- `id` an auto incrementing integer that will come from our database.
- `stripe_id` a string from Stripe that identifies our products.
- `name` the name of our product.
- `price` the price of our product, in USD.

The most important parts to notice are `title` and `properties`. Title is simply what you call the model, and must be globally unique within your organization's models. So, don't create multiple schemas called `product`.

In `properties`, we have 4 fields that match the explanation of our product model above. We won't go into the details, of writing these tags it's pretty self explanatory, and if you do want the details, JSON Schema has awesome documentation.

Next are the remix files. Each data system that you want to pull or push data from needs to have a remix file specifying how data will be remixed on the way in from that system, and how it must be remixed to push data back to it.

Here is the table that we want to sync our data to in PostgreSQL:

```sql
CREATE TABLE products (
    id bigserial PRIMARY KEY,
    stripe_id TEXT UNIQUE,
    product_name TEXT NOT NULL,
    price int,
    -- Any other columns you have internally
);
```

It's OK if your database table has more fields in it, the application won't touch the fields that you don't specify in the remix files.

And here is an example of Stripe's product object:

```json

{
  "id": "prod_NWjs8kKbJWmuuc",
  "object": "product",
  "active": true,
  "created": 1678833149,
  "default_price": "price_1MoBy5LkdIwHu7ixZhnattbh",
  "description": null,
  "images": [],
  "marketing_features": [],
  "livemode": false,
  "metadata": {"id": 3},
  "name": "Gold Plan",
  "package_dimensions": null,
  "shippable": null,
  "statement_descriptor": null,
  "tax_code": null,
  "unit_label": null,
  "updated": 1678833149,
  "url": null
}
```

Notice that Stripe does not call the price of product "price" in their data model. In fact, product's don't have a "price" field at all in Stripe! This is because you can attach many prices to a product to account for different currencies, tax rates, countries, etc. You can have a "default_price", but that field isn't even a number.

In this deceptively unintuitive example, we are translating (or remixing!) between two incompatible data models. With that challenge in mind, let's start creating our remix files, which are written in YAML.

### Example PostgreSQL Remix File

```yaml
prod-db:
  type: "postgresql"
  dsn: "DSN GOES HERE"
  replication_dsn: "DSN FOR CDC CONNECTION GOES HERE"
  max_open_connections: 10
  max_idle_connections: 10
  max_connection_idle_time: 30s
  rate_limit: 10
  rate_bucket_size: 100

  receive_mixer:
    public.products:
      product:
        id:
          field: id
        stripe_id:
          field: stripe_id
        product_name:
          field: name
        price:
          field: price

  push_mixer:
    product:
      public.products:
        id:
          field: id
          search_key: true
        stripe_id:
          field: stripe_id
          search_key: true
        name:
          field: product_name
        price:
          field: price

```

At the top, we've specified connection information, some system configuration like rate limits, and importantly, a `receive_mixer` and `push_mixer`. These two mixers are where Remix gets a bit confusing at first, but also what makes it extremely powerful. It's crucial to understand how these things work.

The receive mixer is used to transform incoming data into objects that can be validated against a JSON Schema, and put into a queue. The push mixer is used to take an object from the queue, change its field names and types to fit into a target system, and push it to that system.

Below is a definition of how receive mixers and push mixers are defined, with the following decoration convention:

- Field names from your models, defined via JSON Schema, are [in square brackets].
- Field names that map to and from from your data system are {in squigly brackets}.
- Static (unchanging) key names are not decorated.

### Mixer definition

```YAML
receive_mixer:
    {location_in_system}:
        [model_title]:
            {field_name_in_system}:
                field: [field_name_in_model]

push_mixer:
    [model_title]:
        {location_in_system}:
            [field_name_in_model]:
                field: {field_name_in_system}
```

In simple English, when the receive router gets a message from a system, it:

1. Checks the location in the system that the change was made (in PostgreSQL's case, the schema and table).
2. Looks up what models should be created because of this data change (it can create more than one object per data change).
3. Maps from the field name in your system, to the field name in your model.

The push router does a similar thing, but in a different order. It:

1. Checks the object to see what model type it is.
2. Looks up where in the system it should push data to.
3. Maps from the field names specified in your model, to the field names expected by that system.

Let's make things concrete by continuing with our example. We are listening for changes to the `public.products` table in PostgreSQL. When a change comes in, we see that we should create a `product` object. Most of the names in our PostgreSQL table are the same as in our model, except `product_name`, which should be `name` in our model. So it maps `product_name` to `name` and leaves everything else the same.

It create a `product` JSON object and validate it against the corresponding JSON Schema file, then put it in the queue.

Conversely, whenever a `product` object is pulled from the queue to put into PostgreSQL, the following process will happen:

1. Checks the object type (in this example, it will be a product type).
2. When a `product` object is found, we need to know what table to sync it to. That will be `public.products`.
3. Then, we need to know what field names in the model correspond to which field names in the `public.products` table. In this case, they are all the same, except we need to rename `name` to `product_name`.
4. Finally, we look for `search_keys` (must be a PK, or at least have a unique index) in order to run upsert / delete commands. You must provide at least one search key per location (such as `public.products`) in the push mixer.

As you can see, there is a lot to understand about mixers, but together they are incredibly powerful. They allow you to work replicate canonical data models between disparate systems which don't conform to your models, and may not call fields the same thing, or even follow the same basic data model! This is super common in legacy situations.

Of course, we need two data systems to actually replicate data. Let's move on to Stripe.

### Example Stripe Remix File

```yaml
payment-gateway:
  type: "stripe"
  use_cli_listener: true
  api_key: "API_KEY_GOES_HERE"
  rate_limit: 10
  rate_bucket_size: 100

  receive_router:
    product:
      product:
          id:
            field: stripe_id
          name:
            field: name
          metadata.id:
            field: id
    price:
      product:
        product:
          field: stripe_id
        unit_amount:
          field: price
    
  push_router:
    product:
      products:
        id:
          field: metadata[id]
        stripe_id:
          field: id
          search_key: true
        name:
          field: name
```

Here, once again we are doing a bit of config at the top of the file. API key, rate limit, etc. One cool feature is that Remix can use the Stripe CLI to forward all webhooks to a locally running API endpoint, negating the need for you to set up and maintain an external URL. Just set `use_cli_listener: true` in the YAML file.

In the case of Stripe, we don't watch a certain location for changes. Instead they are pushed to us via POST request. The two keys `product` and `price`, both on the first level below `receive_router`, correspond to the *object type* that was pushed to us. This means that Remix will listen for both `product` and `price` objects. Remember that Stripe does not allow you to store price data in an object itself - you must create separate price objects. This allows us to keep our prices and products synced across our database and Stripe, despite differing data models!

Another thing of note is that while Remix does not allow nested data structures in your schema models, it *can* accept and push nested objects. Notice the `metadata.id` field being received in the receive router, and the `metadata[id]` field in the push router. Stripe allows you to store arbitrary metadata in an object payload, so here we are storing our auto-incrementing integer ID from our database in Stripe!

The Stripe remix file is doing the following:

- Listening for product and price objects being pushed to us via webhooks.
- When a product object comes in, remix it and create a product object where:
  - id=metadata.id
  - name=name
  - stripe_id=id
- When a price object comes in, remix it and create a product object where:
  - stripe_id=product
  - price=unit_amount

In plain english, it will create a product object whenever it receives a price or product webhook. Because both objects contain something that can be remixed into the `stripe_id` field of our canonical data model, we can then upsert into PostgreSQL and keep everything nice and synced.

Phew! With all that in mind, it's time to start Remix.

## Installing And Running Remix

# postgres-release
---

This is a [BOSH](https://www.bosh.io) release for [PostgreSQL](https://www.postgresql.org/).

# Troubleshooting

## `fly execute`

```
# from ~/workspace/postgres-release
export BOSH_DIRECTOR=10.111.184.230
export BOSH_USER=admin
export BOSH_PASSWORD=********
export DEPLOYMENT_NAME=pgci-cf
fly execute -t pgci --config ci/scripts/run-bosh-delete/task.yml --input=postgres-release=.
```

## `bosh ssh`

1. Log on to pgci-boshinit.softlayer.com (public key already copied there)

1. `bosh ssh` with a limited set of VMs:

    ```
    bosh download manifest pgci-cf-upg1 upg1.yml
    bosh -d upg1.yml ssh
    ```

## Running psql on a postgres VM

1. Log into postgres VM using `bosh ssh`
1. `/var/vcap/packages/postgres-9.4.9/bin/psql -p 5524 -U vcap ccdb`

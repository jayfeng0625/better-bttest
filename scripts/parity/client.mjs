// Clients for one target. Set BIGTABLE_EMULATOR_HOST to target an emulator.
import { Bigtable, v2 } from '@google-cloud/bigtable'

// Every row the cases write starts with this prefix and a run id.
export const ROW_PREFIX = 'probe#'
export const caseKey = (runId, name) => `${ROW_PREFIX}${runId}#${name}`
export const CASE_KEY = new RegExp(`^${ROW_PREFIX}[0-9a-f-]{36}#`)

export const connect = (projectId, instanceId, tableId) => {
    const bigtable = new Bigtable({ projectId })
    const client = new v2.BigtableClient(bigtable.options.BigtableClient)
    return {
        client,
        tableName: client.tablePath(projectId, instanceId, tableId),
        table: bigtable.instance(instanceId).table(tableId),
    }
}

// Delete every row the cases wrote and every table the table cases created, from any run, then confirm that none
// remain. Exits 1 if any do.
// Usage: node cleanup.mjs <project> <instance> <table>
import { CASE_KEY, CASE_TABLE, connect, connectAdmin, ROW_PREFIX } from './client.mjs'

const [projectId, instanceId, tableId] = process.argv.slice(2)
const { table } = connect(projectId, instanceId, tableId)
const { admin, instanceName } = connectAdmin(projectId, instanceId)

const caseRows = async () => {
    const [rows] = await table.getRows({ prefix: ROW_PREFIX, filter: { row: { cellLimit: 1 } } })
    return rows.map((row) => row.id).filter((id) => CASE_KEY.test(id))
}

const caseTables = async () => {
    const [tables] = await admin.listTables({ parent: instanceName, view: 'NAME_ONLY' })
    return tables.map((t) => t.name).filter((name) => CASE_TABLE.test(name))
}

// A case that stops partway can leave its table protected.
const deleteTable = async (name) => {
    const [operation] = await admin.updateTable({ table: { name, deletionProtection: false }, updateMask: { paths: ['deletion_protection'] } })
    await operation.promise()
    await admin.deleteTable({ name })
}

const keys = await caseRows()
if (keys.length) await table.mutate(keys.map((key) => ({ key, method: 'delete' }))).catch((err) => console.error(err.message))
const names = await caseTables()
for (const name of names) await deleteTable(name).catch((err) => console.error(`Cannot delete ${name}: ${err.message}`))

const left = [...(await caseRows()).map((key) => `row ${key}`), ...(await caseTables()).map((name) => `table ${name}`)]
if (left.length) {
    console.error(`Left after cleanup:\n${left.join('\n')}`)
    process.exit(1)
}
console.error(`Deleted ${keys.length} case rows from ${tableId} and ${names.length} case tables.`)

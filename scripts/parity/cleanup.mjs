// Delete every row the cases wrote, from any run, then confirm a prefix scan finds none. Exits 1 if rows remain.
// Usage: node cleanup.mjs <project> <instance> <table>
import { connect, ROW_PREFIX } from './client.mjs'

const [projectId, instanceId, tableId] = process.argv.slice(2)
const { table } = connect(projectId, instanceId, tableId)
const caseRow = new RegExp(`^${ROW_PREFIX}[0-9a-f-]{36}#`)

const caseRows = async () => {
    const [rows] = await table.getRows({ prefix: ROW_PREFIX, filter: { row: { cellLimit: 1 } } })
    return rows.map((row) => row.id).filter((id) => caseRow.test(id))
}

const keys = await caseRows()
if (keys.length) await table.mutate(keys.map((key) => ({ key, method: 'delete' })))
const left = await caseRows()
if (left.length) {
    console.error(`Rows left in ${tableId}:\n${left.join('\n')}`)
    process.exit(1)
}
console.error(`Deleted ${keys.length} case rows from ${tableId}.`)

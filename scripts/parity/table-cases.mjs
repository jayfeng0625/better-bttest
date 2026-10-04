// Run the table admin cases against one target and write the results as JSON, with the run id replaced by <run>.
// Each case creates its own tables and deletes them when it ends. cleanup.mjs deletes any that a failed run leaves.
// Usage: node table-cases.mjs <out.json> <project> <instance>
import { randomBytes } from 'node:crypto'
import { writeFileSync } from 'node:fs'
import { caseTableId, connect, connectAdmin } from './client.mjs'

const [out, projectId, instanceId] = process.argv.slice(2)
const runId = randomBytes(6).toString('hex')
const { admin, instanceName } = connectAdmin(projectId, instanceId)

const FAMILY = 'cf'
const utf8 = (s) => Buffer.from(s, 'utf8')

const delimited = (delimiter, fieldNames) => ({
    fields: fieldNames.map((fieldName) => ({ fieldName, type: { stringType: { encoding: { utf8Bytes: {} } } } })),
    encoding: { delimitedBytes: { delimiter: utf8(delimiter) } },
})
const FOUR_FIELDS = delimited('#', ['a', 'b', 'c', 'd'])

// Production's message for a schema with no encoding continues with text that differs between calls.
const errorOf = (err) => ({ code: err.code, details: String(err.details ?? err.message).replace(/^(Missing encoding for STRUCT).*$/s, '$1') })

const attempt = async (fn) => {
    try {
        await fn()
        return 'ok'
    } catch (err) {
        return errorOf(err)
    }
}

const tableName = (name) => admin.tablePath(projectId, instanceId, caseTableId(runId, name))

const createTable = (name, rowKeySchema) =>
    admin.createTable({
        parent: instanceName,
        tableId: caseTableId(runId, name),
        table: { columnFamilies: { [FAMILY]: { gcRule: { maxNumVersions: 1 } } }, rowKeySchema },
    })

const updateTable = async (name, table, path, ignoreWarnings = false) => {
    const [operation] = await admin.updateTable({ table: { name: tableName(name), ...table }, updateMask: { paths: [path] }, ignoreWarnings })
    await operation.promise()
}

// A missing table gives its code alone. Production's NotFound message differs from the one upstream's GetTable returns.
const getTable = async (name) => {
    try {
        const [table] = await admin.getTable({ name: tableName(name), view: 'SCHEMA_VIEW' })
        return { rowKeySchema: table.rowKeySchema, deletionProtection: table.deletionProtection }
    } catch (err) {
        return { code: err.code }
    }
}

const NOT_FOUND = 5

const cases = {}
let tables = 0
// Run one case. fn gets newTable, which names a table for the case to create. Each named table is deleted afterwards.
const run = async (name, fn) => {
    const names = []
    const newTable = () => {
        names.push(`t${++tables}`)
        return names.at(-1)
    }
    try {
        cases[name] = await fn(newTable)
    } catch (err) {
        cases[name] = { caseError: errorOf(err) }
    }
    for (const n of names) {
        await admin.deleteTable({ name: tableName(n) }).catch((err) => {
            if (err.code !== NOT_FOUND) console.error(`Cannot delete ${tableName(n)}: ${err.message}`)
        })
    }
    writeFileSync(out, JSON.stringify(cases, null, 2).replaceAll(runId, '<run>'))
}

const tableWith = async (newTable, rowKeySchema) => {
    const name = newTable()
    await createTable(name, rowKeySchema)
    return name
}

// Send the update without ignore_warnings, then with it, reading the table after each.
const updateTwice = async (name, rowKeySchema) => {
    const out = []
    for (const ignoreWarnings of [false, true]) {
        const update = await attempt(() => updateTable(name, { rowKeySchema }, 'row_key_schema', ignoreWarnings))
        out.push({ ignoreWarnings, update, table: await getTable(name) })
    }
    return out
}

const schemas = [
    ['four fields', FOUR_FIELDS],
    ['a different delimiter', delimited('|', ['a', 'b', 'c', 'd'])],
    ['one field', delimited('#', ['a'])],
    ['no fields', delimited('#', [])],
    ['no encoding', {}],
]

for (const [label, schema] of schemas) {
    await run(`CreateTable with a row key schema with ${label}`, async (newTable) => {
        const name = newTable()
        return { create: await attempt(() => createTable(name, schema)), table: await getTable(name) }
    })
}

// UpdateTable on a table with the four-field schema, and on one with no schema. No schema leaves the field unset.
for (const [label, schema] of [...schemas, ['no schema', undefined]]) {
    await run(`UpdateTable row_key_schema with ${label}, on a table with four fields`, async (newTable) =>
        updateTwice(await tableWith(newTable, FOUR_FIELDS), schema),
    )
    await run(`UpdateTable row_key_schema with ${label}, on a table with no schema`, async (newTable) =>
        updateTwice(await tableWith(newTable, undefined), schema),
    )
}

await run('UpdateTable row_key_schema on a protected table', async (newTable) => {
    const name = await tableWith(newTable, undefined)
    await updateTable(name, { deletionProtection: true }, 'deletion_protection')
    try {
        const set = await attempt(() => updateTable(name, { rowKeySchema: FOUR_FIELDS }, 'row_key_schema'))
        const afterSet = await getTable(name)
        const clear = await attempt(() => updateTable(name, {}, 'row_key_schema', true))
        return { set, afterSet, clear, afterClear: await getTable(name) }
    } finally {
        await updateTable(name, { deletionProtection: false }, 'deletion_protection')
    }
})

await run('Writes with keys that do not fit the row key schema', async (newTable) => {
    const name = await tableWith(newTable, FOUR_FIELDS)
    const { client, tableName: dataTable } = connect(projectId, instanceId, caseTableId(runId, name))
    const keys = [utf8('a#b#c#d#e'), utf8('a'), Buffer.from([0x61, 0x23, 0xff, 0xfe, 0x23, 0x63, 0x23, 0x64])]
    const writes = []
    for (const rowKey of keys) {
        const mutations = [{ setCell: { familyName: FAMILY, columnQualifier: utf8('q'), value: utf8('v'), timestampMicros: 1000 } }]
        writes.push(await attempt(() => client.mutateRow({ tableName: dataTable, rowKey, mutations })))
    }
    const rows = []
    for await (const response of client.readRows({ tableName: dataTable })) {
        for (const chunk of response.chunks) {
            if (chunk.rowKey?.length) rows.push(Buffer.from(chunk.rowKey).toString('hex'))
        }
    }
    return { writes, rows }
})

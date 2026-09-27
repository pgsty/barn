import Foundation
import CryptoKit
import Security
import Darwin

struct RunnerError: Error, LocalizedError {
    let code: String
    let message: String
    var errorDescription: String? { message }
    init(_ code: String, _ message: String) { self.code = code; self.message = message }
}

func errorObject(_ error: Error) -> [String: Any] {
    if let e = error as? RunnerError { return ["ok": false, "error": ["code": e.code, "message": e.message]] }
    let ns = error as NSError
    return ["ok": false, "error": ["code": "\(ns.domain):\(ns.code)", "message": ns.localizedDescription]]
}

func jsonData(_ object: [String: Any]) throws -> Data {
    var data = try JSONSerialization.data(withJSONObject: object, options: [.sortedKeys, .withoutEscapingSlashes])
    data.append(0x0a)
    return data
}

func emit(_ object: [String: Any], to handle: FileHandle = .standardOutput) {
    if let data = try? jsonData(object) { try? handle.write(contentsOf: data) }
}

func progress(_ phase: String, _ detail: [String: Any] = [:]) {
    var object = detail
    object["phase"] = phase
    object["time"] = ISO8601DateFormatter().string(from: Date())
    emit(object, to: .standardError)
}

func writePrivate(_ data: Data, to url: URL, mode: mode_t = 0o600) throws {
    try data.write(to: url, options: [.atomic])
    guard chmod(url.path, mode) == 0 else { throw RunnerError("file_permissions", "Cannot secure \(url.lastPathComponent)") }
}

func writeJSON(_ value: [String: Any], to url: URL, mode: mode_t = 0o600) throws {
    try writePrivate(jsonData(value), to: url, mode: mode)
}

func object(from data: Data) throws -> [String: Any] {
    guard let value = try JSONSerialization.jsonObject(with: data) as? [String: Any] else {
        throw RunnerError("invalid_json", "Expected a JSON object")
    }
    return value
}

func digest(_ data: Data) -> String { SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined() }

struct Arguments {
    let command: String
    var positional: [String] = []
    var values: [String: String] = [:]
    var flags: Set<String> = []
    init(_ raw: [String]) throws {
        command = raw.first ?? "help"
        var i = 1
        while i < raw.count {
            let name = raw[i]
            if !name.hasPrefix("--") { positional.append(name); i += 1; continue }
            if ["--force", "--help", "--nat"].contains(name) {
                guard flags.insert(name).inserted else { throw RunnerError("argument", "Repeated \(name)") }
                i += 1; continue
            }
            guard i + 1 < raw.count, !raw[i + 1].hasPrefix("--"), values[name] == nil else {
                throw RunnerError("argument", "Missing or repeated value for \(name)")
            }
            values[name] = raw[i + 1]; i += 2
        }
    }
    func string(_ name: String) throws -> String {
        guard let value = values[name], !value.isEmpty else { throw RunnerError("argument", "\(name) is required") }
        return value
    }
    func path(_ name: String) throws -> URL { URL(fileURLWithPath: try string(name)).standardizedFileURL }
    func integer(_ name: String, default fallback: Int) throws -> Int {
        guard let value = values[name] else { return fallback }
        guard let result = Int(value), result > 0 else { throw RunnerError("argument", "\(name) must be a positive integer") }
        return result
    }
    func validate(values allowed: Set<String>, flags allowedFlags: Set<String> = [], positionals: Int = 0) throws {
        guard positional.count == positionals else { throw RunnerError("argument", "Unexpected positional arguments") }
        if let bad = Set(values.keys).subtracting(allowed).first { throw RunnerError("argument", "Unknown argument \(bad)") }
        if let bad = flags.subtracting(allowedFlags).first { throw RunnerError("argument", "Unknown flag \(bad)") }
    }
}

func readLineJSON(fd: Int32, maximum: Int = 65536) throws -> [String: Any] {
    var data = Data()
    while data.count < maximum {
        var byte: UInt8 = 0
        let count = Darwin.read(fd, &byte, 1)
        if count == 0 { break }
        if count < 0 { if errno == EINTR { continue }; throw RunnerError("socket_read", String(cString: strerror(errno))) }
        if byte == 0x0a { return try object(from: data) }
        data.append(byte)
    }
    if data.isEmpty { return [:] }
    guard data.count < maximum else { throw RunnerError("request_too_large", "Request exceeds 64 KiB") }
    return try object(from: data)
}

func sendJSON(_ value: [String: Any], fd: Int32) throws {
    let data = try jsonData(value)
    try data.withUnsafeBytes { raw in
        var written = 0
        while written < raw.count {
            let count = Darwin.write(fd, raw.baseAddress!.advanced(by: written), raw.count - written)
            if count < 0 { if errno == EINTR { continue }; throw RunnerError("socket_write", String(cString: strerror(errno))) }
            written += count
        }
    }
}

func keychain(_ args: Arguments) throws -> [String: Any] {
    try args.validate(values: ["--service", "--account"], positionals: 1)
    let service = try args.string("--service"), account = try args.string("--account")
    guard service.hasPrefix("farrow.mac."), UUID(uuidString: account) != nil else {
        throw RunnerError("argument", "Keychain service must begin farrow.mac. and account must be an instance UUID")
    }
    var query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: account]
    let operation = args.positional[0]
    var status: OSStatus
    switch operation {
    case "set":
        let input = try readLineJSON(fd: STDIN_FILENO)
        guard let password = input["password"] as? String, !password.isEmpty else { throw RunnerError("argument", "stdin password is required") }
        let attributes: [String: Any] = [kSecValueData as String: Data(password.utf8)]
        status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if status == errSecItemNotFound {
            query[kSecValueData as String] = Data(password.utf8)
            query[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
            status = SecItemAdd(query as CFDictionary, nil)
        }
    case "get":
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        status = SecItemCopyMatching(query as CFDictionary, &item)
        if status == errSecSuccess, let data = item as? Data, let password = String(data: data, encoding: .utf8) {
            return ["ok": true, "password": password]
        }
    case "delete":
        status = SecItemDelete(query as CFDictionary)
        if status == errSecItemNotFound { status = errSecSuccess }
    default: throw RunnerError("argument", "secret expects set, get, or delete")
    }
    guard status == errSecSuccess else {
        throw RunnerError("keychain:\(status)", (SecCopyErrorMessageString(status, nil) as String?) ?? "Keychain operation failed")
    }
    return ["ok": true]
}

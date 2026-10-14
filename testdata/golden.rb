# Regenerates the golden files the Go catalog tests compare against by running
# the original Rails services from djm-mono over testdata/csvs and
# testdata/search.
#
#   ruby testdata/golden.rb [path/to/djm-mono]
#
# Needs Ruby with the activesupport, csv and sqlite3 gems; Rails itself is
# stubbed out.

require "active_support/all"
require "csv"
require "fileutils"
require "json"
require "logger"
require "pathname"
require "sqlite3"
require "tmpdir"

HERE = Pathname(__dir__)
MONO = Pathname(ARGV[0] || HERE.join("../../djm-mono").to_s).expand_path
TMP = Pathname(Dir.mktmpdir("djm-golden"))
at_exit { FileUtils.rm_rf(TMP) }

module Rails
  def self.logger = (@logger ||= Logger.new(nil))
  def self.env = ActiveSupport::StringInquirer.new("development")
  def self.root = TMP
end

module Djm
  module Metrics
    def self.enabled? = false
  end
end

%w[catalog_csv_encoding csv_validator catalog_db_builder catalog_search_service].each do |name|
  load MONO.join("songs/app/services/#{name}.rb").to_s
end

Record = Struct.new(:catalog_id, :active_db_version) do
  def db_path(version = active_db_version)
    TMP.join("storage", "catalog_dbs", "catalog_#{catalog_id}_v#{version}.db")
  end
end

# Mirrors BuildCatalogJob#build_catalog without ActiveRecord.
def import(path, record)
  File.open(path, "rb") do |file|
    prepared =
      begin
        CatalogCsvEncoding.prepare!(file)
      rescue CatalogCsvEncoding::CatalogCsvEncodingError
        return { "status" => "failed",
                 "errors" => [ { type: "encoding_error", detail: CatalogCsvEncoding::USER_FRIENDLY_DECODE_ERROR } ] }
      end

    CatalogCsvEncoding.rewind_prepared(prepared)
    validation = CsvValidator.new(prepared.csv_io).validate
    unless validation.valid?
      return { "status" => "failed", "source_encoding" => prepared.source_encoding, "errors" => validation.errors }
    end

    CatalogCsvEncoding.rewind_prepared(prepared)
    version = record.active_db_version.to_i + 1
    count = CatalogDbBuilder.new(record, version, csv_io: prepared.csv_io).build
    record.active_db_version = version

    db = SQLite3::Database.new(record.db_path.to_s, readonly: true)
    songs = db.execute("SELECT id, title, artist, album, versions_json, external_ids_json, versions_text FROM songs ORDER BY id").map do |id, title, artist, album, versions, ids, text|
      { id: id, title: title, artist: artist, album: album,
        versions: versions && JSON.parse(versions), external_ids: ids && JSON.parse(ids), versions_text: text }
    end
    db.close

    { "status" => "ready", "source_encoding" => prepared.source_encoding, "song_count" => count,
      "replacement_stats" => validation.replacement_stats, "songs" => songs }
  end
rescue => e
  # Rails surfaced these as a generic job failure.
  { "status" => "failed", "exception" => e.class.name, "message" => e.message }
end

def write(path, data)
  File.write(path, JSON.pretty_generate(data) + "\n")
end

Dir[HERE.join("csvs/*.csv").to_s].sort.each_with_index do |csv, i|
  write(csv.sub(/\.csv\z/, ".json"), import(csv, Record.new(i + 1, nil)))
end

search_record = Record.new(9999, nil)
built = import(HERE.join("search/catalog.csv").to_s, search_record)
raise "search catalog failed to build: #{built.inspect}" unless built["status"] == "ready"

service = CatalogSearchService.new(search_record)
queries = JSON.parse(HERE.join("search/queries.json").read)
results = queries.map do |q|
  r = service.search(q["q"], limit: q["limit"], offset: q["offset"], sort: q["sort"])
  q.merge("total" => r.total, "result_sort" => r.sort, "songs" => r.songs)
end
write(HERE.join("search/golden.json"), results)

# Random short inputs for the CSV reader: records exactly as Ruby's
# liberal_parsing returns them (nil for unquoted empty fields), plus the
# error message if parsing stops.
rng = Random.new(42)
alphabet = [ "a", "b", ",", ",", "\"", "\"", "\n", "\r", " " ]
inputs = [ "", "\n", "\r\n", "\r", "a", "\"\"", "a,b\r\n\r\nc" ]
inputs += Array.new(5000) { Array.new(rng.rand(1..10)) { alphabet.sample(random: rng) }.join }
cases = inputs.uniq.map do |input|
  records = []
  error = nil
  begin
    CSV.new(input, liberal_parsing: true).each { |r| records << r }
  rescue CSV::MalformedCSVError => e
    error = e.message
  end
  { input: input, records: records, error: error }
end
write(HERE.join("csvparse.json"), cases)

puts "wrote #{Dir[HERE.join('csvs/*.json').to_s].size} csv goldens, #{results.size} search cases, #{cases.size} parser cases"

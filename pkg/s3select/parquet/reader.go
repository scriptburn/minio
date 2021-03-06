/*
 * Minio Cloud Storage, (C) 2019 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package parquet

import (
	"io"

	parquetgo "github.com/minio/parquet-go"
	parquetgen "github.com/minio/parquet-go/gen-go/parquet"
	"github.com/scriptburn/minio/pkg/s3select/json"
	"github.com/scriptburn/minio/pkg/s3select/sql"
)

// Reader - Parquet record reader for S3Select.
type Reader struct {
	args *ReaderArgs
	file *parquetgo.File
}

// Read - reads single record.
func (r *Reader) Read() (sql.Record, error) {
	parquetRecord, err := r.file.Read()
	if err != nil {
		if err != io.EOF {
			return nil, errParquetParsingError(err)
		}

		return nil, err
	}

	record := json.NewRecord()
	for name, v := range parquetRecord {
		if v.Value == nil {
			if err = record.Set(name, sql.NewNull()); err != nil {
				return nil, errParquetParsingError(err)
			}

			continue
		}

		var value *sql.Value
		switch v.Type {
		case parquetgen.Type_BOOLEAN:
			value = sql.NewBool(v.Value.(bool))
		case parquetgen.Type_INT32:
			value = sql.NewInt(int64(v.Value.(int32)))
		case parquetgen.Type_INT64:
			value = sql.NewInt(v.Value.(int64))
		case parquetgen.Type_FLOAT:
			value = sql.NewFloat(float64(v.Value.(float32)))
		case parquetgen.Type_DOUBLE:
			value = sql.NewFloat(v.Value.(float64))
		case parquetgen.Type_INT96, parquetgen.Type_BYTE_ARRAY, parquetgen.Type_FIXED_LEN_BYTE_ARRAY:
			value = sql.NewString(string(v.Value.([]byte)))
		default:
			return nil, errParquetParsingError(nil)
		}

		if err = record.Set(name, value); err != nil {
			return nil, errParquetParsingError(err)
		}
	}

	return record, nil
}

// Close - closes underlaying readers.
func (r *Reader) Close() error {
	return r.file.Close()
}

// NewReader - creates new Parquet reader using readerFunc callback.
func NewReader(getReaderFunc func(offset, length int64) (io.ReadCloser, error), args *ReaderArgs) (*Reader, error) {
	file, err := parquetgo.Open(getReaderFunc, nil)
	if err != nil {
		if err != io.EOF {
			return nil, errParquetParsingError(err)
		}

		return nil, err
	}

	return &Reader{
		args: args,
		file: file,
	}, nil
}

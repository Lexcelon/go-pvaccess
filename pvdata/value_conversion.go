package pvdata

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

func parseTag(tag string) (name string, tags map[string]string) {
	if len(tag) > 0 {
		tags = make(map[string]string)
		pairs := strings.Split(tag, ",")
		name = pairs[0]
		pairs = pairs[1:]
		for _, pair := range pairs {
			if parts := strings.SplitN(pair, "=", 2); len(parts) == 2 {
				tags[parts[0]] = parts[1]
			} else {
				tags[pair] = ""
			}
		}
	}
	return
}

type option func(v reflect.Value) PVField

func alwaysOption(val int64) option {
	return func(v reflect.Value) PVField {
		if v.Kind() == reflect.Ptr && v.Elem().CanSet() {
			switch v.Elem().Kind() {
			case reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				v.Elem().SetInt(int64(val))
			case reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				v.Elem().SetUint(uint64(val))
			}
		}
		return nil
	}
}
func boundOption(bound int64) option {
	return func(v reflect.Value) PVField {
		if v.CanInterface() {
			if i, ok := v.Interface().(*string); ok {
				return &PVBoundedString{
					(*PVString)(i),
					PVSize(bound),
				}
			}
		}
		return nil
	}
}
func nameOption(name string) option {
	return func(v reflect.Value) PVField {
		if v.Kind() == reflect.Ptr && v.Elem().Kind() == reflect.Struct {
			return PVStructure{ID: name, v: v.Elem()}
		}
		return nil
	}
}
func shortOption() option {
	return func(v reflect.Value) PVField {
		if v.Kind() == reflect.Ptr {
			v = v.Elem()
		}
		if v.Kind() == reflect.Slice {
			return PVArray{alwaysShort: true, v: v}
		}
		return nil
	}
}

func tagsToOptions(tags map[string]string) []option {
	var options []option
	if val, ok := tags["name"]; ok {
		options = append(options, nameOption(val))
	}
	if val, ok := tags["always"]; ok {
		if val, err := strconv.ParseInt(val, 0, 64); err == nil {
			options = append(options, alwaysOption(val))
		}
	}
	if val, ok := tags["bound"]; ok {
		if val, err := strconv.ParseInt(val, 0, 64); err == nil {
			options = append(options, boundOption(val))
		}
	}
	if _, ok := tags["short"]; ok {
		options = append(options, shortOption())
	}
	return options
}

type TypeIDer interface {
	TypeID() string
}

func valueToPVField(v reflect.Value, options ...option) PVField {
	fmt.Println("\nOptions:\n", options)
	for _, o := range options {
		if pvf := o(v); pvf != nil {
			return pvf
		}
	}
	//fmt.Println("\nvalueToPVField Before PTR check 1\n", v.Type(), v.Kind(), v.CanInterface())
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	fmt.Println("\nvalueToPVField after PTR check 1\n", v.Type(), v.Kind(), v.CanInterface())
	if v.CanInterface() {
		i := v.Interface()
		//fmt.Println("INTERFACE:", i)

		if i, ok := i.(PVField); ok {
			return i
		}
		fmt.Println("\nvalueToPVField Before PTR check 2\n", i, v)
		if v.Kind() != reflect.Ptr && v.CanAddr() {
			i = v.Addr().Interface()
		}
		fmt.Println("\nvalueToPVField after PTR check 2\n", i)
		if i, ok := i.(PVField); ok {
			return i
		}
		fmt.Println("\nAttempting to cast to PVField\n")
		switch i := i.(type) {
		case *PVField:
			fmt.Println("\nSelected PVField\n")
			return *i
		case *bool:
			fmt.Println("\nSelected bool\n")
			return (*PVBoolean)(i)
		case *int8:
			fmt.Println("\nSelected int8\n")
			return (*PVByte)(i)
		case *uint8:
			fmt.Println("\nSelected uint8\n")
			return (*PVUByte)(i)
		case *int16:
			fmt.Println("\nSelected int16\n")
			return (*PVShort)(i)
		case *uint16:
			fmt.Println("\nSelected uint16\n")
			return (*PVUShort)(i)
		case *int32:
			fmt.Println("\nSelected int32\n")
			return (*PVInt)(i)
		case *uint32:
			fmt.Println("\nSelected uint32\n")
			return (*PVUInt)(i)
		case *int64:
			fmt.Println("\nSelected int64\n")
			return (*PVLong)(i)
		case *uint64:
			fmt.Println("\nSelected uint64\n")
			return (*PVULong)(i)
		case *float32:
			fmt.Println("\nSelected float32\n")
			return (*PVFloat)(i)
		case *float64:
			fmt.Println("\nSelected float64\n")
			return (*PVDouble)(i)
		case *string:
			fmt.Println("\nSelected string\n")
			return (*PVString)(i)
		}
		//fmt.Println("\nFailed to cast to PVField\n")
	}
	//fmt.Println("\nvalueToPVField Before PTR check 3\n", v.Type(), v.Kind(), v.CanInterface())
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}
	//fmt.Println("\nvalueToPVField after PTR check 3\n", v.Type(), v.Kind(), v.CanInterface())

	//fmt.Println("\nAttempting to cast to PVField Array\n")
	switch v.Kind() {
	case reflect.Slice:
		return PVArray{v: v}
	case reflect.Array:
		return PVArray{fixed: true, v: v}
	case reflect.Struct:
		var typeID string
		if v, ok := v.Interface().(TypeIDer); ok {
			typeID = v.TypeID()
		}
		return PVStructure{ID: typeID, v: v}
	}
	//fmt.Println("\nFailed to cast to PVField Array\n")
	return nil
}

// Encode writes vs to s.Buf.
// All items in vs must implement PVField or be a pointer to something that can be converted to a PVField.
func Encode(s *EncoderState, vs ...interface{}) error {
	for _, v := range vs {
		pvf := valueToPVField(reflect.ValueOf(v))
		if pvf == nil {
			return fmt.Errorf("can't encode %T %+v", v, v)
		}
		if err := pvf.PVEncode(s); err != nil {
			return err
		}
	}
	return nil
}
func Decode(s *DecoderState, vs ...interface{}) error {
	//fmt.Println("\n-- DECODE START --\n")
	fmt.Println("\nVS:\n", vs)
	for _, v := range vs {
		//fmt.Print("Index: ", i)
		fmt.Println("\nVS VALUE:\n", reflect.ValueOf(vs))
		//fmt.Println("\nV\n", v)
		//fmt.Println("\nV VALUE:\n", reflect.ValueOf(v), reflect.ValueOf(v).Type())
		if reflect.ValueOf(v).Kind() == reflect.Ptr {
			//fmt.Println("\nV VALUE DEEP\n", reflect.ValueOf(v).Elem())
		}
		pvf := valueToPVField(reflect.ValueOf(v))
		if reflect.ValueOf(pvf).Kind() == reflect.Ptr {
			//fmt.Println("\nPTR:\n", reflect.ValueOf(pvf))
		} else {
			//fmt.Println("\nREFLECTED:\n", pvf)
		}

		if pvf == nil {
			//fmt.Println("\nCAN'T DECODE\n", v)
			return fmt.Errorf("can't decode %#v", v)
		}
		//fmt.Println("\nTYPE OF DECODED\n", reflect.TypeOf(pvf))
		if err := pvf.PVDecode(s); err != nil {
			//fmt.Println("\nERROR DECODING\n", err)
			return err
		}
		//fmt.Println("\nSuccess DECODED\n", reflect.ValueOf(pvf))
	}
	//fmt.Println("\n----------------END THIS DECODE-------------------\n")
	return nil
}

type FieldDescer interface {
	FieldDesc() (FieldDesc, error)
}

func valueToField(v reflect.Value) (FieldDesc, error) {
	if f, ok := v.Interface().(FieldDescer); ok {
		return f.FieldDesc()
	}
	pvf := valueToPVField(v)
	if f, ok := pvf.(FieldDescer); ok {
		return f.FieldDesc()
	}
	return FieldDesc{}, fmt.Errorf("don't know how to describe %#v", v.Interface())
}

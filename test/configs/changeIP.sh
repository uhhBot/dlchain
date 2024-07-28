for entry in "."/*.txt
do
  sed -i 's/127.0.0.1/0.0.0.0/g' "$entry"
    # echo "$entry"
done
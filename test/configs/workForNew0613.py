import os
from pickle import FALSE

import argparse

parser = argparse.ArgumentParser(description='Specify nodes info.')
parser.add_argument('shard_size', metavar='shard_size', type=int)
parser.add_argument('shard_num', metavar='shard_num', type=int,
                    help='except beacon chain')
parser.add_argument('machine_num', metavar='machine_num', type=int
                    )
parser.add_argument('--r', action="store_true",
                    help='replace the local nodes file')
args = parser.parse_args()

# 分片大小
shard_size = args.shard_size
# 分片数量（包括beacon，记得＋1）
shard_num = args.shard_num + 1
# 运行机器数量
machine_num = args.machine_num

dir_path = os.path.dirname(os.path.realpath(__file__))

opened_list = []
for i in range(int(machine_num)) :
    if args.r:
        opened_list.append(open('./test/configs/local-resharding.txt', 'w'))
    else :
        opened_list.append(open(dir_path+'/diy'+ str(shard_num-1) +'shard'+str(shard_size)+'sizeMachine#'+ str(i) +'.txt', 'w'))
srouce_file = open(dir_path+'/largerepo.txt', 'r')
Lines = srouce_file.readlines()

cnt = 0
for i in range(len(Lines)) :

    if(i % shard_num == 0):
        # 忽略beacon chain
        continue
    isLeader = False
    if(i / shard_num == 0):
        # 标记leader
        isLeader = True
         
     
    # check this line
    # index = Lines[i].find("validator")
    # rightPart = Lines[i][index:] 
    # port 是在一台机器上用的
    port = 9000 + cnt//machine_num
    leftPart = "0.0.0.0" + " " + str(port)+ " validator " 
    # if isLeader == True:
    #     leftPart = "leader 0.0.0.0" + " " + str(port)+ " validator " 
    #  " line_ "+ str(i)  +"  " + 
    opened_list[cnt % machine_num].writelines([leftPart + ".hmy/diykeys_new/" + Lines[i][:-1]+".key\n"])
    cnt += 1
    if cnt == (shard_num-1)*shard_size:
        break
for i in opened_list:
    i.close()
    print(i.name)
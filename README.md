# gokcp
gokcp是KCP (https://github.com/skywind3000/kcp) 的Go语言实现。

gokcp与KCP的不同点在于：  
1. 以位计算方式实现了ack list，好处是简化代码以及节约一个int32的大小。  
2. 实现另外一种RTO的计算方法，算法移植自Linux内核里面TCP RTO计算算法，在慢网速但丢包少的环境下，此算法能少重传大约个位数百分比的数据包。  

# 如何使用
请参考goocp: https://github.com/shaoyuan1943/goocp